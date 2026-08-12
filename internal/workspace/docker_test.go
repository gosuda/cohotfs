package workspace

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type lifecycleBackend struct {
	created          runtime.WorkspaceSpec
	createCalls      int
	ref              runtime.WorkspaceRef
	status           runtime.WorkspaceStatus
	started          bool
	startCalls       int
	stopped          bool
	deleted          bool
	deleteCalls      int
	hostKeyReads     int
	hostKeyFailures  int
	readyFingerprint string
	readyReads       int
	readyFailures    int
	cleanupSystem    int
	setupCalls       int
	setupExitCode    int
	hostKey          []byte
	onDelete         func() error
	socketListeners  []*net.UnixListener
}

func (f *lifecycleBackend) Probe(context.Context) (runtime.BackendInfo, error) {
	return availableDocker(), nil
}
func (f *lifecycleBackend) Pull(context.Context, runtime.PullRequest) (runtime.ResolvedImage, error) {
	return runtime.ResolvedImage{}, nil
}
func (f *lifecycleBackend) Create(_ context.Context, spec runtime.WorkspaceSpec) (runtime.WorkspaceRef, error) {
	f.createCalls++
	f.created = spec
	f.ref = runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": "container"}, Nonce: spec.CreationNonce}
	f.status = runtime.WorkspaceStatus{Exists: true, Labels: spec.Labels}
	return f.ref, nil
}
func (f *lifecycleBackend) Start(_ context.Context, _ runtime.WorkspaceRef) error {
	f.startCalls++
	for _, declared := range f.created.Mounts {
		if declared.Target != "/run/cohotfs/host/ssh" {
			continue
		}
		socketPath := filepath.Join(declared.Source, "ssh.sock")
		_ = os.Remove(socketPath)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			return err
		}
		if err := os.Chmod(socketPath, 0o600); err != nil {
			_ = listener.Close()
			return err
		}
		f.socketListeners = append(f.socketListeners, listener)
	}
	f.started = true
	f.status.Running = true
	return nil
}
func (f *lifecycleBackend) ensureHostKey() error {
	if f.hostKey != nil {
		return nil
	}
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		return err
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		return err
	}
	f.hostKey = ssh.MarshalAuthorizedKey(key)
	return nil
}

func (f *lifecycleBackend) ExecSync(_ context.Context, _ runtime.WorkspaceRef, request runtime.ExecRequest) (runtime.ExecResult, error) {
	if len(request.Argv) == 2 && request.Argv[0] == containerAgentExecutable && request.Argv[1] == "ready" {
		f.readyReads++
		if f.readyFailures > 0 {
			f.readyFailures--
			return runtime.ExecResult{ExitCode: 1}, nil
		}
		if err := f.ensureHostKey(); err != nil {
			return runtime.ExecResult{}, err
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey(f.hostKey)
		if err != nil {
			return runtime.ExecResult{}, err
		}
		fingerprint := ssh.FingerprintSHA256(key)
		if f.readyFingerprint != "" {
			fingerprint = f.readyFingerprint
		}
		output, err := json.Marshal(containeragent.Ready{
			BootstrapAPI: containeragent.BootstrapAPI, SSHAddress: "127.0.0.1:2222", SSHHostFingerprint: fingerprint,
			SSHRelay: true, TCPForwarding: true,
		})
		if err != nil {
			return runtime.ExecResult{}, err
		}
		return runtime.ExecResult{Output: append(output, '\n')}, nil
	}
	if len(request.Argv) == 2 && request.Argv[0] == "/bin/cat" && request.Argv[1] == containerSSHHostPublicKey {
		f.hostKeyReads++
		if f.hostKeyFailures > 0 {
			f.hostKeyFailures--
			return runtime.ExecResult{ExitCode: 1}, nil
		}
		if err := f.ensureHostKey(); err != nil {
			return runtime.ExecResult{}, err
		}
		return runtime.ExecResult{Output: append([]byte(nil), f.hostKey...)}, nil
	}
	if len(request.Argv) >= 2 && request.Argv[0] == containerAgentExecutable && request.Argv[1] == "setup" {
		f.setupCalls++
		return runtime.ExecResult{ExitCode: f.setupExitCode, Output: []byte("setup output")}, nil
	}
	if len(request.Argv) == 4 && request.Argv[0] == containerAgentExecutable && request.Argv[1] == "cleanup-system" && request.Argv[2] == "--fingerprint" {
		f.cleanupSystem++
		f.hostKey = nil
		return runtime.ExecResult{}, nil
	}
	if len(request.Argv) == 4 && request.Argv[0] == "/bin/rm" && request.Argv[2] == containerSSHHostPrivateKey && request.Argv[3] == containerSSHHostPublicKey {
		f.hostKey = nil
		return runtime.ExecResult{}, nil
	}
	return runtime.ExecResult{}, nil
}
func (f *lifecycleBackend) Inspect(context.Context, runtime.WorkspaceRef) (runtime.WorkspaceStatus, error) {
	return f.status, nil
}
func (f *lifecycleBackend) Stop(context.Context, runtime.WorkspaceRef, time.Duration) error {
	f.stopped = true
	f.status.Running = false
	for _, listener := range f.socketListeners {
		_ = listener.Close()
	}
	f.socketListeners = nil
	return nil
}
func (f *lifecycleBackend) Delete(context.Context, runtime.WorkspaceRef) error {
	if f.onDelete != nil {
		if err := f.onDelete(); err != nil {
			return err
		}
	}
	f.deleted = true
	f.deleteCalls++
	f.status.Exists = false
	return nil
}

func testSSHSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ssh.sock")
}

func TestDockerServiceCreateStartStopRemove(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Setup.Mode = "manual"
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	createRequest := CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(), ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey, MaskCohotfsRoot: true}
	record, err := service.Create(context.Background(), createRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Create(context.Background(), createRequest)
	if err != nil || replayed.ID != record.ID || backend.createCalls != 1 {
		t.Fatalf("create replay = %#v, calls=%d, err=%v", replayed, backend.createCalls, err)
	}
	changedRequest := createRequest
	changedRequest.MaskCohotfsRoot = false
	if _, err := service.Create(context.Background(), changedRequest); err == nil || apperr.Code(err) != apperr.ExitStateConflict {
		t.Fatalf("changed create replay error = %v", err)
	}
	if record.Status != state.StatusStopped || record.RuntimeRef.IDs["container"] != "container" || backend.created.Labels[LabelCreationNonce] == "" || record.BootstrapAPI != containeragent.BootstrapAPI || record.TCPForwarding {
		t.Fatalf("created record=%#v request=%#v", record, backend.created)
	}
	var foundCohotfsMask, foundHome, foundSystem bool
	for _, mounted := range backend.created.Mounts {
		switch mounted.Target {
		case "/workspace/.cohotfs":
			foundCohotfsMask = mounted.Type == "tmpfs" && mounted.Source == "" && mounted.ReadOnly
		case "/home/agent":
			foundHome = mounted.Source == filepath.Join(root.Path(), "workspaces", record.ID, "home") && mounted.Type == "bind" && mounted.Propagation == "rprivate" && !mounted.ReadOnly
		case "/var/lib/cohotfs/system":
			foundSystem = mounted.Source == filepath.Join(root.Path(), "workspaces", record.ID, "system") && mounted.Type == "bind" && mounted.Propagation == "rprivate" && !mounted.ReadOnly
		}
	}
	if !foundCohotfsMask || !foundHome || !foundSystem {
		t.Fatalf("persistent or mask mounts missing: %#v", backend.created.Mounts)
	}
	for _, name := range []string{"home", "system"} {
		info, err := os.Stat(filepath.Join(root.Path(), "workspaces", record.ID, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s backing directory = %#v, %v", name, info, err)
		}
	}
	planRaw, err := store.LoadWorkspaceArtifact(record.ID, "plan.json")
	if err != nil || len(planRaw) == 0 {
		t.Fatalf("plan = %q, %v", planRaw, err)
	}
	startKey := t.Name() + "/start"
	record, err = service.Start(context.Background(), record.ID, startKey)
	if err != nil || record.Status != state.StatusReady || !record.TCPForwarding || backend.startCalls != 1 {
		t.Fatalf("start = %#v, calls=%d, err=%v", record, backend.startCalls, err)
	}
	replayed, err = service.Start(context.Background(), record.ID, startKey)
	if err != nil || replayed.Status != state.StatusReady || backend.startCalls != 1 {
		t.Fatalf("start replay = %#v, calls=%d, err=%v", replayed, backend.startCalls, err)
	}
	restartKey := t.Name() + "/restart"
	if _, replay, err := store.BeginOperation(record.ID, restartKey, "workspace.restart", []byte(`{"timeout":"1s"}`), time.Now()); err != nil || replay {
		t.Fatalf("begin interrupted restart: replay=%v err=%v", replay, err)
	}
	record.Status = state.StatusStarting
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	restarted, err := service.Restart(context.Background(), record.ID, restartKey, time.Second)
	if err != nil || restarted.Status != state.StatusReady || backend.stopped {
		t.Fatalf("restart resume = %#v, stopped=%v, err=%v", restarted, backend.stopped, err)
	}
	record = restarted
	record, err = service.Stop(context.Background(), record.ID, t.Name()+"/stop", time.Second)
	if err != nil || record.Status != state.StatusStopped || !backend.stopped {
		t.Fatalf("stop = %#v, %v", record, err)
	}
	removeKey := t.Name() + "/remove"
	if err := service.Remove(context.Background(), record.ID, removeKey); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(context.Background(), record.ID, removeKey); err != nil {
		t.Fatalf("remove replay: %v", err)
	}
	if !backend.deleted || backend.deleteCalls != 1 || backend.cleanupSystem != 1 {
		t.Fatalf("backend deleted=%v delete calls=%d system cleanups=%d", backend.deleted, backend.deleteCalls, backend.cleanupSystem)
	}
	if _, err := store.LoadWorkspace(record.ID); !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
		t.Fatalf("state remains after remove: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root.Path(), "workspaces", record.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("persistent workspace storage remains after remove: %v", err)
	}
}

func TestDockerServiceRemoveRecoversReadyWorkspaceWithMissingContainer(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := state.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	record := state.Workspace{
		ID: id, OwnerUID: 1000, ManifestDigest: "manifest", Backend: "docker", Status: state.StatusReady,
		RuntimeRef: runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": "missing"}, Nonce: "nonce"},
	}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{status: runtime.WorkspaceStatus{Exists: false}}
	service := NewDockerService(root, store, backend)
	if err := service.Remove(context.Background(), id, t.Name()+"/remove"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadWorkspace(id); !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
		t.Fatalf("recovered state remains: %v", err)
	}
}

func TestDockerServicePersistsRemovingBeforeContainerDelete(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := state.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	ref := runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": "container"}, Nonce: "nonce"}
	record := state.Workspace{ID: id, OwnerUID: 1000, ManifestDigest: "manifest", Backend: "docker", Status: state.StatusReady, RuntimeRef: ref}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{status: runtime.WorkspaceStatus{Exists: true, Running: true, Labels: map[string]string{
		LabelOwnerUID: "1000", LabelWorkspaceID: id, LabelManifest: "manifest", LabelCreationNonce: "nonce",
	}}}
	backend.onDelete = func() error {
		persisted, err := store.LoadWorkspace(id)
		if err != nil {
			return err
		}
		if persisted.Status != state.StatusRemoving {
			return fmt.Errorf("state at container delete = %s", persisted.Status)
		}
		return nil
	}
	service := NewDockerService(root, store, backend)
	if err := service.Remove(context.Background(), id, t.Name()+"/remove"); err != nil {
		t.Fatal(err)
	}
}

func TestDockerServiceRotatesAndRepinsSSHHostKey(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Setup.Mode = "manual"
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	record, err := service.Create(context.Background(), CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(), ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	record, err = service.Start(context.Background(), record.ID, t.Name()+"/start")
	if err != nil {
		t.Fatal(err)
	}
	previous := record.SSHHostFingerprint
	record, err = service.RotateSSHHostKey(context.Background(), record.ID, t.Name()+"/rotate")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != state.StatusReady || record.SSHHostFingerprint == "" || record.SSHHostFingerprint == previous {
		t.Fatalf("rotated workspace = %#v", record)
	}
	var active, released int
	for _, resource := range record.Resources {
		if resource.Type != "ssh_known_hosts" {
			continue
		}
		if resource.ReleasedAt == nil {
			active++
		} else {
			released++
		}
	}
	if active != 1 || released != 1 {
		t.Fatalf("known-host resources active=%d released=%d", active, released)
	}
}

func TestDockerServiceWaitsForSSHHostKeyGeneration(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{readyFailures: 2, hostKeyFailures: 2}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Setup.Mode = "manual"
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	record, err := service.Create(context.Background(), CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(), ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	record, err = service.Start(context.Background(), record.ID, t.Name()+"/start")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != state.StatusReady || backend.readyReads != 3 || backend.hostKeyReads != 3 {
		t.Fatalf("record=%#v readiness reads=%d host key reads=%d", record, backend.readyReads, backend.hostKeyReads)
	}
}

func TestDockerServiceRejectsReadinessHostKeyMismatch(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{readyFingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Setup.Mode = "manual"
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	record, err := service.Create(context.Background(), CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(), ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	record, err = service.Start(context.Background(), record.ID, t.Name()+"/start")
	if err == nil || record.Status != state.StatusError || !backend.stopped {
		t.Fatalf("mismatched readiness key record=%#v stopped=%v err=%v", record, backend.stopped, err)
	}
}

func TestDockerServiceAutomaticSetupModes(t *testing.T) {
	for _, mode := range []string{"once", "always", "manual"} {
		t.Run(mode, func(t *testing.T) {
			root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			store, err := state.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			source := t.TempDir()
			if err := os.MkdirAll(filepath.Join(source, ".cohotfs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, ".cohotfs", "setup.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			backend := &lifecycleBackend{}
			service := NewDockerService(root, store, backend)
			workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
			workspace.Spec.Setup.Mode = mode
			publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
			if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
			record, err := service.Create(context.Background(), CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: source, ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey})
			if err != nil {
				t.Fatal(err)
			}
			record, err = service.Start(context.Background(), record.ID, t.Name()+"/start-1")
			if err != nil || record.Status != state.StatusReady {
				t.Fatalf("first start record=%#v err=%v", record, err)
			}
			record, err = service.Stop(context.Background(), record.ID, t.Name()+"/stop", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			record, err = service.Start(context.Background(), record.ID, t.Name()+"/start-2")
			if err != nil || record.Status != state.StatusReady {
				t.Fatalf("second start record=%#v err=%v", record, err)
			}
			want := map[string]int{"once": 1, "always": 2, "manual": 0}[mode]
			if backend.setupCalls != want {
				t.Fatalf("setup calls=%d, want %d", backend.setupCalls, want)
			}
		})
	}
}

func TestDockerServiceAutomaticSetupFailureStopsWorkspace(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, ".cohotfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".cohotfs", "setup.sh"), []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{setupExitCode: 9}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	record, err := service.Create(context.Background(), CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: source, ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	record, err = service.Start(context.Background(), record.ID, t.Name()+"/start")
	if err == nil || record.Status != state.StatusSetupFailed || !backend.stopped || backend.status.Running {
		t.Fatalf("failed setup record=%#v stopped=%v running=%v err=%v", record, backend.stopped, backend.status.Running, err)
	}
}

func TestCreateOperationBodyExcludesSecretsAndVolatileProbeMetadata(t *testing.T) {
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{
		Workspace:       config.BuiltinWorkspace("api", "example.invalid/base:dev"),
		CanonicalSource: "/source", ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000,
		Image:           runtime.ResolvedImage{Reference: "example.invalid/base:dev", Digest: "sha256:digest", BootstrapAPI: containeragent.BootstrapAPI, ResolvedAt: time.Unix(1, 0)},
		BackendInfo:     runtime.BackendInfo{Name: "docker", Version: "one", Endpoint: "unix:///one", Available: true, Capabilities: map[runtime.Capability]bool{runtime.CapabilityHostSocketBind: true}},
		BootstrapSource: publicKey, Environment: []string{"HOME=/home/user", "SECRET_TOKEN=first"},
	}
	first, err := createOperationBody(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Environment = []string{"HOME=/home/user", "SECRET_TOKEN=second"}
	request.Image.ResolvedAt = time.Unix(2, 0)
	request.BackendInfo.Version = "two"
	request.BackendInfo.Endpoint = "unix:///two"
	second, err := createOperationBody(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || strings.Contains(string(first), "SECRET_TOKEN") || strings.Contains(string(first), "first") {
		t.Fatalf("volatile or secret input affected operation body: first=%s second=%s", first, second)
	}
	request.Environment = []string{"HOME=/different"}
	changed, err := createOperationBody(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) == string(first) {
		t.Fatal("agent seed environment change did not affect operation body")
	}
}

func TestDockerServiceResumesPersistedCreatingOperation(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		persistRuntime  bool
		tamperBootstrap bool
		wantCreateCalls int
		wantErrorCode   int
	}{
		{name: "before-plan", wantCreateCalls: 1},
		{name: "recorded-runtime", persistRuntime: true, wantCreateCalls: 0},
		{name: "tampered-bootstrap", persistRuntime: true, tamperBootstrap: true, wantErrorCode: apperr.ExitPartialCleanup},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			store, err := state.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			backend := &lifecycleBackend{}
			service := NewDockerService(root, store, backend)
			workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
			workspace.Spec.Setup.Mode = "manual"
			publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
			if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			request := CreateRequest{
				OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(),
				ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000,
				Image: runtime.ResolvedImage{
					Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI,
				},
				BackendInfo: availableDocker(), BootstrapSource: publicKey,
				SSHSocketPath: testSSHSocket(t),
			}
			id, err := state.WorkspaceIDForOperationKey(request.OperationKey)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			record := state.Workspace{
				ID: id, Name: workspace.Metadata.Name, OwnerUID: request.OwnerUID, OwnerGID: request.OwnerGID,
				CanonicalSource: request.CanonicalSource, ManifestDigest: request.ManifestDigest, Backend: "docker",
				ImageDigest: request.Image.Digest, BootstrapAPI: request.Image.BootstrapAPI, ContainerUID: request.OwnerUID, ContainerGID: request.OwnerGID,
				Status: state.StatusCreating, CreatedAt: now, UpdatedAt: now,
			}
			if testCase.persistRuntime {
				plan, err := CompilePlan(workspace, id, request.OwnerUID, request.OwnerGID, request.CanonicalSource, request.ManifestDigest, request.Image, "", request.SSHSocketPath, request.BackendInfo)
				if err != nil {
					t.Fatal(err)
				}
				persistentMounts, err := service.preparePersistentStorage(id)
				if err != nil {
					t.Fatal(err)
				}
				plan.Mounts = append(plan.Mounts, persistentMounts...)
				if _, err := service.prepareBootstrap(id, publicKey, &plan, nil); err != nil {
					t.Fatal(err)
				}
				if testCase.tamperBootstrap {
					relative := filepath.Join("workspaces", id, "bootstrap", "bootstrap.json")
					raw, err := root.ReadFile(relative)
					if err != nil {
						t.Fatal(err)
					}
					tampered := strings.Replace(string(raw), `"enableCDP": false`, `"enableCDP": true`, 1)
					if tampered == string(raw) {
						t.Fatal("bootstrap fixture had no disabled CDP grant")
					}
					if err := root.AtomicWrite(relative, []byte(tampered), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				planRaw, err := RedactedPlan(plan)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveWorkspaceArtifact(id, "plan.json", append(planRaw, '\n')); err != nil {
					t.Fatal(err)
				}
				record.RuntimeRef = runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": "persisted-container"}, Nonce: plan.CreationNonce}
				backend.status = runtime.WorkspaceStatus{Exists: true, Labels: map[string]string{
					LabelOwnerUID: "1000", LabelWorkspaceID: id, LabelManifest: request.ManifestDigest, LabelCreationNonce: plan.CreationNonce,
				}}
			}
			if err := store.SaveWorkspace(record); err != nil {
				t.Fatal(err)
			}
			body, err := createOperationBody(request)
			if err != nil {
				t.Fatal(err)
			}
			if _, replay, err := store.BeginOperation(id, request.OperationKey, "workspace.create", body, now); err != nil || replay {
				t.Fatalf("begin interrupted create: replay=%v err=%v", replay, err)
			}

			resumed, err := service.Create(context.Background(), request)
			if testCase.wantErrorCode != 0 {
				if apperr.Code(err) != testCase.wantErrorCode || resumed.Status != state.StatusError || !resumed.Quarantined || backend.createCalls != 0 {
					t.Fatalf("resumed=%#v createCalls=%d err=%v", resumed, backend.createCalls, err)
				}
				return
			}
			if err != nil || resumed.Status != state.StatusStopped || resumed.ID != id || backend.createCalls != testCase.wantCreateCalls {
				t.Fatalf("resumed=%#v createCalls=%d err=%v", resumed, backend.createCalls, err)
			}
			replayed, err := service.Create(context.Background(), request)
			if err != nil || replayed.Status != state.StatusStopped || backend.createCalls != testCase.wantCreateCalls {
				t.Fatalf("replayed=%#v createCalls=%d err=%v", replayed, backend.createCalls, err)
			}
		})
	}
}

func TestDockerServiceQuarantinesCreatingRuntimeWithoutPlan(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Setup.Mode = "manual"
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{
		OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(),
		ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000,
		Image: runtime.ResolvedImage{
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI,
		},
		BackendInfo: availableDocker(), BootstrapSource: publicKey,
		SSHSocketPath: testSSHSocket(t),
	}
	id, err := state.WorkspaceIDForOperationKey(request.OperationKey)
	if err != nil {
		t.Fatal(err)
	}
	nonce := "persisted-nonce"
	now := time.Now().UTC()
	record := state.Workspace{
		ID: id, Name: workspace.Metadata.Name, OwnerUID: request.OwnerUID, OwnerGID: request.OwnerGID,
		CanonicalSource: request.CanonicalSource, ManifestDigest: request.ManifestDigest, Backend: "docker",
		ImageDigest: request.Image.Digest, ContainerUID: request.OwnerUID, ContainerGID: request.OwnerGID,
		RuntimeRef: runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": "persisted-container"}, Nonce: nonce},
		Status:     state.StatusCreating, CreatedAt: now, UpdatedAt: now,
	}
	backend.status = runtime.WorkspaceStatus{Exists: true, Labels: map[string]string{
		LabelOwnerUID: "1000", LabelWorkspaceID: id, LabelManifest: request.ManifestDigest, LabelCreationNonce: nonce,
	}}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	body, err := createOperationBody(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, replay, err := store.BeginOperation(id, request.OperationKey, "workspace.create", body, now); err != nil || replay {
		t.Fatalf("begin interrupted create: replay=%v err=%v", replay, err)
	}
	resumed, err := service.Create(context.Background(), request)
	if apperr.Code(err) != apperr.ExitPartialCleanup || resumed.Status != state.StatusError || !resumed.Quarantined || backend.createCalls != 0 {
		t.Fatalf("resumed=%#v createCalls=%d err=%v", resumed, backend.createCalls, err)
	}
}
