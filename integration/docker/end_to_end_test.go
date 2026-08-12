//go:build integration && linux

package docker_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	cohotruntime "github.com/gosuda/cohotfs/internal/runtime"
	runtimedocker "github.com/gosuda/cohotfs/internal/runtime/docker"
	setupservice "github.com/gosuda/cohotfs/internal/setup"
	"github.com/gosuda/cohotfs/internal/sshproxy"
	"github.com/gosuda/cohotfs/internal/state"
	workspaceservice "github.com/gosuda/cohotfs/internal/workspace"
	"github.com/moby/moby/api/types/container"
)

const proxyHelperEnvironment = "COHOTFS_INTEGRATION_SSH_PROXY"

func TestWorkspaceEndToEnd(t *testing.T) {
	if os.Getuid() == 0 || os.Getgid() == 0 {
		t.Fatal("Docker integration requires a non-root invoking user")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	harness := newHarness(t, ctx)

	t.Run("once-directory-ssh", func(t *testing.T) {
		record, workspace, source := harness.createWorkspace(t, "it-once", "once", true, config.ResourceSpec{})
		record = harness.start(t, record)
		assertTransport(t, record, "ssh_socket")
		assertFile(t, filepath.Join(source, "setup-count"), "1\n")
		stdout, stderr := harness.ssh(t, record, nil, "cat /workspace/setup-result")
		if string(stdout) != "ok\n" || len(stderr) != 0 {
			t.Fatalf("SSH setup result stdout=%q stderr=%q", stdout, stderr)
		}

		payload := make([]byte, 64<<10)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		stdout, stderr = harness.ssh(t, record, payload, "cat > /workspace/random.bin && cat /workspace/random.bin")
		if !bytes.Equal(stdout, payload) || len(stderr) != 0 {
			t.Fatalf("SSH changed binary stream: got=%d want=%d stderr=%q", len(stdout), len(payload), stderr)
		}
		harness.assertPortForward(t, record, source)
		harness.copyWithSCP(t, record, payload, "/workspace/scp.bin")
		harness.copyWithSFTP(t, record, payload, "/workspace/sftp.bin")
		for _, path := range []string{"/workspace/scp.bin", "/workspace/sftp.bin"} {
			stdout, _ = harness.ssh(t, record, nil, "cat "+path)
			if !bytes.Equal(stdout, payload) {
				t.Fatalf("%s digest=%s want=%s", path, digest(stdout), digest(payload))
			}
		}

		knownHosts := activeResourcePath(t, record, "ssh_known_hosts")
		originalKnownHosts, err := os.ReadFile(knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		clientPublicKey, err := os.ReadFile(harness.privateKey + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		replacement := "cohotfs-" + record.ID + " " + strings.TrimSpace(string(clientPublicKey)) + "\n"
		if err := os.WriteFile(knownHosts, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(knownHosts, originalKnownHosts, 0o600)
		sshPath, err := exec.LookPath("ssh")
		if err != nil {
			t.Fatal(err)
		}
		rejectCtx, rejectCancel := context.WithTimeout(ctx, 10*time.Second)
		arguments := append(harness.sshOptions(t, record), "-T", "agent@cohotfs-"+record.ID, "true")
		output, rejectErr := exec.CommandContext(rejectCtx, sshPath, arguments...).CombinedOutput()
		rejectCancel()
		if rejectErr == nil || !bytes.Contains(output, []byte("HOST IDENTIFICATION HAS CHANGED")) {
			t.Fatalf("replaced host key was not rejected: err=%v output=%q", rejectErr, output)
		}
		if err := os.WriteFile(knownHosts, originalKnownHosts, 0o600); err != nil {
			t.Fatal(err)
		}

		record = harness.stop(t, record)
		record = harness.start(t, record)
		assertFile(t, filepath.Join(source, "setup-count"), "1\n")
		validation, err := setupservice.Validate(source, workspace.Spec.Setup, record.ImageDigest, harness.image.BootstrapAPI, record.OwnerUID, record.OwnerGID)
		if err != nil {
			t.Fatal(err)
		}
		record, err = setupservice.NewService(harness.store, harness.adapter).Run(ctx, record.ID, workspace.Spec.Setup, validation, true, true)
		if err != nil || record.Status != state.StatusReady {
			t.Fatalf("force setup record=%#v err=%v", record, err)
		}
		assertFile(t, filepath.Join(source, "setup-count"), "2\n")
		harness.remove(t, record)
	})

	t.Run("loopback-fallback-disabled", func(t *testing.T) {
		if harness.info.Capabilities[cohotruntime.Capability("loopback_publish")] || harness.info.Capabilities[cohotruntime.Capability("managed_network")] {
			t.Fatal("Docker advertised an unverified network path")
		}
		workspace := config.BuiltinWorkspace("it-unsupported", harness.tag)
		_, err := workspaceservice.CompilePlan(workspace, "workspace", os.Getuid(), os.Getgid(), t.TempDir(), "manifest", harness.image, "", "", harness.info)
		if err == nil || !cohotruntime.IsUnsupported(err) {
			t.Fatalf("missing directory transport error = %v", err)
		}
	})

	t.Run("always-directory-ssh", func(t *testing.T) {
		record, _, source := harness.createWorkspace(t, "it-always", "always", true, config.ResourceSpec{})
		record = harness.start(t, record)
		assertTransport(t, record, "ssh_socket")
		assertFile(t, filepath.Join(source, "setup-count"), "1\n")
		record = harness.stop(t, record)
		record = harness.start(t, record)
		assertTransport(t, record, "ssh_socket")
		assertFile(t, filepath.Join(source, "setup-count"), "2\n")
		harness.remove(t, record)
	})

	t.Run("manual", func(t *testing.T) {
		record, workspace, source := harness.createWorkspace(t, "it-manual", "manual", true, config.ResourceSpec{})
		record = harness.start(t, record)
		if _, err := os.Stat(filepath.Join(source, "setup-result")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("manual setup ran automatically: %v", err)
		}
		validation, err := setupservice.Validate(source, workspace.Spec.Setup, record.ImageDigest, harness.image.BootstrapAPI, record.OwnerUID, record.OwnerGID)
		if err != nil {
			t.Fatal(err)
		}
		record, err = setupservice.NewService(harness.store, harness.adapter).Run(ctx, record.ID, workspace.Spec.Setup, validation, true, false)
		if err != nil || record.Status != state.StatusReady {
			t.Fatalf("manual setup record=%#v err=%v", record, err)
		}
		assertFile(t, filepath.Join(source, "setup-count"), "1\n")
		harness.remove(t, record)
	})

	t.Run("concurrent-mutation", func(t *testing.T) {
		record, _, _ := harness.createWorkspace(t, "it-concurrent", "manual", true, config.ResourceSpec{})
		lock, err := harness.store.LockWorkspace(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, startErr := harness.service.Start(ctx, record.ID, harness.operationKey("blocked-start"))
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if apperr.Code(startErr) != apperr.ExitStateConflict {
			t.Fatalf("concurrent start error=%v code=%d", startErr, apperr.Code(startErr))
		}
		record = harness.start(t, record)
		harness.remove(t, record)
	})

	t.Run("resource-policy", func(t *testing.T) {
		cases := []struct {
			name      string
			resources config.ResourceSpec
		}{
			{name: "default"},
			{name: "recommended", resources: config.ResourceSpec{Enabled: true, CPU: 2, Memory: 4 << 30, MemorySwap: 5 << 30, PIDs: 512, Nofile: config.NofileLimit{Soft: 1024, Hard: 4096}}},
			{name: "large", resources: config.ResourceSpec{Enabled: true, CPU: 8, Memory: 32 << 30, MemorySwap: 64 << 30, PIDs: 4096, Nofile: config.NofileLimit{Soft: 65536, Hard: 65536}}},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				record, _, _ := harness.createWorkspace(t, "it-resource-"+testCase.name, "manual", true, testCase.resources)
				hostConfig := harness.inspectHostConfig(t, record)
				assertResources(t, hostConfig, testCase.resources)
				harness.remove(t, record)
			})
		}
	})
}

// TestSSHProxyHelper is executed by OpenSSH ProxyCommand. os.Exit prevents the
// Go test protocol from contaminating the raw SSH stream on stdout.
func TestSSHProxyHelper(t *testing.T) {
	if os.Getenv(proxyHelperEnvironment) != "1" {
		return
	}
	root, err := hostroot.OpenForTest(os.Getenv("COHOTFS_INTEGRATION_ROOT"))
	if err == nil {
		var store *state.Store
		store, err = state.NewStore(root)
		if err == nil {
			workspaceID := os.Args[len(os.Args)-1]
			err = sshproxy.Proxy(context.Background(), store, workspaceID, os.Stdin, os.Stdout)
		}
		_ = root.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type harness struct {
	t          *testing.T
	ctx        context.Context
	base       string
	home       string
	endpoint   string
	dockerPath string
	tag        string
	root       *hostroot.Root
	store      *state.Store
	adapter    *runtimedocker.Adapter
	service    *workspaceservice.DockerService
	info       cohotruntime.BackendInfo
	image      cohotruntime.ResolvedImage
	privateKey string
	sequence   int
	tracked    map[string]struct{}
}

func newHarness(t *testing.T, ctx context.Context) *harness {
	t.Helper()
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("Docker integration prerequisite: %v", err)
	}
	endpoint, err := integrationDockerEndpoint(dockerPath)
	if err != nil {
		t.Fatalf("Docker integration endpoint: %v", err)
	}
	adapter, err := runtimedocker.New(endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := adapter.Probe(ctx)
	if err != nil || !info.Available {
		t.Fatalf("local Docker Engine is not reachable at %s: %v", endpoint, err)
	}
	base, err := os.MkdirTemp("/tmp", "cohotfs-it-")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := hostroot.OpenForTest(filepath.Join(home, ".cohotfs"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.EnsureDir("ssh", 0o700); err != nil {
		t.Fatal(err)
	}
	privateKey, _ := root.HostPath("ssh/id_ed25519")
	keygen := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKey)
	if output, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("generate integration SSH key: %v: %s", err, output)
	}
	h := &harness{t: t, ctx: ctx, base: base, home: home, endpoint: endpoint, dockerPath: dockerPath, root: root, store: store, adapter: adapter, info: info, privateKey: privateKey, tracked: map[string]struct{}{}}
	h.service = workspaceservice.NewDockerService(root, store, adapter)
	t.Cleanup(h.close)
	h.tag = "cohotfs-integration:" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h.image = h.buildImage(t)
	return h
}

func (h *harness) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var cleanupErrors []error
	for id := range h.tracked {
		if _, err := h.store.LoadWorkspace(id); err == nil {
			if err := h.service.Remove(ctx, id, h.operationKey("cleanup-"+id)); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("remove workspace %s: %w", id, err))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("load workspace %s for cleanup: %w", id, err))
		}
		_, _ = h.runDockerContext(ctx, "container", "rm", "-f", "cohotfs-"+id)
	}
	if h.tag != "" {
		_, _ = h.runDockerContext(ctx, "image", "rm", "-f", h.tag)
	}
	if h.root != nil {
		if err := h.root.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close host root: %w", err))
		}
	}
	if err := os.RemoveAll(h.base); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove integration root: %w", err))
	}
	if err := errors.Join(cleanupErrors...); err != nil {
		h.t.Errorf("integration cleanup: %v", err)
	}
}

func (h *harness) buildImage(t *testing.T) cohotruntime.ResolvedImage {
	t.Helper()
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("locate repository root")
	}
	repository := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	contextDirectory := filepath.Join(h.base, "image")
	if err := os.MkdirAll(contextDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(contextDirectory, "cohotfs-agent")
	buildAgent := exec.CommandContext(h.ctx, "go", "build", "-trimpath", "-o", agentPath, "./cmd/cohotfs-agent")
	buildAgent.Dir = repository
	buildAgent.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, err := buildAgent.CombinedOutput(); err != nil {
		t.Fatalf("build cohotfs-agent: %v: %s", err, output)
	}
	containerfile, err := os.ReadFile(filepath.Join(repository, "images", "workspace-base", "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDirectory, "Containerfile"), containerfile, 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	agentDigest := sha256.Sum256(agent)
	image, events, buildErr := h.adapter.Build(h.ctx, cohotruntime.BuildRequest{
		Context: contextDirectory, Containerfile: "Containerfile", Tags: []string{h.tag},
		Args:           map[string]string{"AGENT_SHA256": hex.EncodeToString(agentDigest[:]), "COHOTFS_VERSION": "integration"},
		PermittedRoots: []string{contextDirectory}, CohotfsRoot: h.root.Path(),
	})
	var transcript strings.Builder
	for event := range events {
		transcript.WriteString(event.Message)
		if event.Error != "" {
			transcript.WriteString(event.Error)
			transcript.WriteByte('\n')
		}
	}
	if buildErr != nil {
		t.Fatalf("build workspace image: %v\n%s", buildErr, transcript.String())
	}
	image, err = h.adapter.CheckCompatibility(h.ctx, image)
	if err != nil {
		t.Fatalf("workspace image compatibility: %v", err)
	}
	return image
}

func (h *harness) operationKey(operation string) string {
	h.sequence++
	return fmt.Sprintf("integration/%d/%s", h.sequence, operation)
}

func (h *harness) createWorkspace(t *testing.T, name, mode string, directorySocket bool, resources config.ResourceSpec) (state.Workspace, config.Workspace, string) {
	t.Helper()
	source := filepath.Join(h.home, "projects", name)
	if err := os.MkdirAll(filepath.Join(source, ".cohotfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
count=0
if [ -f /workspace/setup-count ]; then count=$(cat /workspace/setup-count); fi
count=$((count + 1))
printf '%s\n' "$count" > /workspace/setup-count
printf 'ok\n' > /workspace/setup-result
`
	if err := os.WriteFile(filepath.Join(source, ".cohotfs", "setup.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := config.BuiltinWorkspace(name, h.tag)
	workspace.Spec.Setup.Mode = mode
	workspace.Spec.Setup.Timeout = time.Minute
	workspace.Spec.Resources = resources
	raw, err := config.Render(workspace)
	if err != nil {
		t.Fatal(err)
	}
	operationKey := h.operationKey("create-" + name)
	id, err := state.WorkspaceIDForOperationKey(operationKey)
	if err != nil {
		t.Fatal(err)
	}
	h.tracked[id] = struct{}{}
	var socketPath string
	if directorySocket {
		directory := filepath.Join("run", "workspaces", id)
		if err := h.root.EnsureDir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		socketPath, err = h.root.SocketPath(filepath.Join(directory, "ssh.sock"))
		if err != nil {
			t.Fatal(err)
		}
	}
	publicKey := h.privateKey + ".pub"
	record, err := h.service.Create(h.ctx, workspaceservice.CreateRequest{
		OperationKey: operationKey, Workspace: workspace, CanonicalSource: source,
		ManifestDigest: workspaceservice.ManifestDigest(raw), OwnerUID: os.Getuid(), OwnerGID: os.Getgid(),
		Image: h.image, BackendInfo: h.info, SSHSocketPath: socketPath, BootstrapSource: publicKey,
	})
	if err != nil || record.Status != state.StatusStopped {
		t.Fatalf("create %s record=%#v err=%v", name, record, err)
	}
	return record, workspace, source
}

func (h *harness) start(t *testing.T, record state.Workspace) state.Workspace {
	t.Helper()
	started, err := h.service.Start(h.ctx, record.ID, h.operationKey("start-"+record.ID))
	if err != nil || started.Status != state.StatusReady {
		t.Fatalf("start record=%#v err=%v", started, err)
	}
	return started
}

func (h *harness) stop(t *testing.T, record state.Workspace) state.Workspace {
	t.Helper()
	stopped, err := h.service.Stop(h.ctx, record.ID, h.operationKey("stop-"+record.ID), 10*time.Second)
	if err != nil || stopped.Status != state.StatusStopped {
		t.Fatalf("stop record=%#v err=%v", stopped, err)
	}
	return stopped
}

func (h *harness) remove(t *testing.T, record state.Workspace) {
	t.Helper()
	ref := record.RuntimeRef
	if err := h.service.Remove(h.ctx, record.ID, h.operationKey("remove-"+record.ID)); err != nil {
		t.Fatalf("remove %s: %v", record.ID, err)
	}
	status, err := h.adapter.Inspect(h.ctx, ref)
	if err != nil || status.Exists {
		t.Fatalf("container remained after remove: %#v, %v", status, err)
	}
	if _, err := h.store.LoadWorkspace(record.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace state remained after remove: %v", err)
	}
	for _, relative := range []string{filepath.Join("run", "workspaces", record.ID), filepath.Join("workspaces", record.ID)} {
		path, _ := h.root.HostPath(relative)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("host artifact %s remained: %v", path, err)
		}
	}
	delete(h.tracked, record.ID)
}

func (h *harness) ssh(t *testing.T, record state.Workspace, stdin []byte, remoteCommand string) ([]byte, []byte) {
	t.Helper()
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	arguments := append(h.sshOptions(t, record), "-T", "agent@cohotfs-"+record.ID, remoteCommand)
	command := exec.CommandContext(h.ctx, sshPath, arguments...)
	command.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		containerID := record.RuntimeRef.IDs["container"]
		logs, _ := h.runDocker("container", "logs", containerID)
		details, _ := h.runDocker("container", "exec", containerID, "/bin/sh", "-c",
			"getent passwd agent; passwd -S agent; for path in /var/lib/cohotfs /var/lib/cohotfs/system /var/lib/cohotfs/system/authorized_keys /home/agent; do stat -c '%A %u:%g %n' \"$path\"; done")
		t.Fatalf("ssh %q: %v: %s\ncontainer logs:\n%s\ncontainer identity:\n%s", remoteCommand, err, stderr.String(), logs, details)
	}
	return stdout.Bytes(), stderr.Bytes()
}

func (h *harness) sshOptions(t *testing.T, record state.Workspace) []string {
	t.Helper()
	knownHosts := ""
	for _, resource := range record.Resources {
		if resource.Type == "ssh_known_hosts" && resource.ReleasedAt == nil {
			knownHosts = resource.Identity["path"]
		}
	}
	if knownHosts == "" {
		t.Fatal("known-host record is missing")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	proxy := "env " + proxyHelperEnvironment + "=1 COHOTFS_INTEGRATION_ROOT=" + shellQuote(h.root.Path()) + " " + shellQuote(executable) + " -test.run=^TestSSHProxyHelper$ -- " + shellQuote(record.ID)
	return []string{
		"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "LogLevel=ERROR", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "ProxyCommand=" + proxy, "-i", h.privateKey,
	}
}

func (h *harness) assertPortForward(t *testing.T, record state.Workspace, source string) {
	t.Helper()
	const containerPort = 39001
	echoSource := filepath.Join(source, "loopback-echo.go")
	echoBinary := filepath.Join(source, "loopback-echo")
	program := `package main

import (
	"io"
	"net"
)

func main() {
	listener, err := net.Listen("tcp4", "127.0.0.1:39001")
	if err != nil {
		panic(err)
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			panic(err)
		}
		go func() {
			defer connection.Close()
			_, _ = io.Copy(connection, connection)
		}()
	}
}
`
	if err := os.WriteFile(echoSource, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(h.ctx, "go", "build", "-trimpath", "-o", echoBinary, echoSource)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build loopback echo server: %v: %s", err, output)
	}

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancelServer := context.WithCancel(h.ctx)
	serverArguments := append(h.sshOptions(t, record), "-T", "agent@cohotfs-"+record.ID, "exec /workspace/loopback-echo")
	server := exec.CommandContext(serverCtx, sshPath, serverArguments...)
	var serverOutput bytes.Buffer
	server.Stdout = &serverOutput
	server.Stderr = &serverOutput
	if err := server.Start(); err != nil {
		cancelServer()
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	defer stopIntegrationCommand(cancelServer, server, serverDone)

	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localPort := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	forwardCtx, cancelForward := context.WithCancel(h.ctx)
	forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, containerPort)
	forwardArguments := append(h.sshOptions(t, record),
		"-T", "-N", "-o", "ExitOnForwardFailure=yes",
		"-L", forward, "agent@cohotfs-"+record.ID,
	)
	forwardCommand := exec.CommandContext(forwardCtx, sshPath, forwardArguments...)
	var forwardOutput bytes.Buffer
	forwardCommand.Stdout = &forwardOutput
	forwardCommand.Stderr = &forwardOutput
	if err := forwardCommand.Start(); err != nil {
		cancelForward()
		t.Fatal(err)
	}
	forwardDone := make(chan error, 1)
	go func() { forwardDone <- forwardCommand.Wait() }()
	defer stopIntegrationCommand(cancelForward, forwardCommand, forwardDone)

	payload := []byte("cohotfs localhost forwarding\n")
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-serverDone:
			serverDone <- err
			t.Fatalf("loopback echo server exited: %v: %s", err, serverOutput.String())
		case err := <-forwardDone:
			forwardDone <- err
			t.Fatalf("OpenSSH port forward exited: %v: %s", err, forwardOutput.String())
		default:
		}
		connection, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", localPort), 200*time.Millisecond)
		if err == nil {
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			echoed := make([]byte, len(payload))
			_, writeErr := connection.Write(payload)
			_, readErr := io.ReadFull(connection, echoed)
			_ = connection.Close()
			if writeErr == nil && readErr == nil && bytes.Equal(echoed, payload) {
				return
			}
			lastErr = errors.Join(writeErr, readErr)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("localhost forward did not echo payload: %v\nserver: %s\nforward: %s", lastErr, serverOutput.String(), forwardOutput.String())
}

func stopIntegrationCommand(cancel context.CancelFunc, command *exec.Cmd, done <-chan error) {
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		<-done
	}
}

func (h *harness) copyWithSCP(t *testing.T, record state.Workspace, payload []byte, destination string) {
	t.Helper()
	path := filepath.Join(h.base, "scp.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := append(h.sshOptions(t, record), path, "agent@cohotfs-"+record.ID+":"+destination)
	command := exec.CommandContext(h.ctx, "scp", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("scp: %v: %s", err, output)
	}
}

func (h *harness) copyWithSFTP(t *testing.T, record state.Workspace, payload []byte, destination string) {
	t.Helper()
	path := filepath.Join(h.base, "sftp.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	batch := filepath.Join(h.base, "sftp.batch")
	if err := os.WriteFile(batch, []byte("put "+path+" "+destination+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := append([]string{"-b", batch}, h.sshOptions(t, record)...)
	arguments = append(arguments, "agent@cohotfs-"+record.ID)
	command := exec.CommandContext(h.ctx, "sftp", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sftp: %v: %s", err, output)
	}
}

func (h *harness) inspectHostConfig(t *testing.T, record state.Workspace) container.HostConfig {
	t.Helper()
	output, err := h.runDocker("container", "inspect", "--format", "{{json .HostConfig}}", record.RuntimeRef.IDs["container"])
	if err != nil {
		t.Fatalf("inspect Docker host config: %v: %s", err, output)
	}
	var hostConfig container.HostConfig
	if err := json.Unmarshal(output, &hostConfig); err != nil {
		t.Fatalf("decode Docker host config: %v: %s", err, output)
	}
	return hostConfig
}

func (h *harness) runDocker(arguments ...string) ([]byte, error) {
	return h.runDockerContext(h.ctx, arguments...)
}

func (h *harness) runDockerContext(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, h.dockerPath, arguments...)
	command.Env = environmentWith("DOCKER_HOST", h.endpoint)
	return command.CombinedOutput()
}

func integrationDockerEndpoint(dockerPath string) (string, error) {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		command := exec.Command(dockerPath, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("inspect current Docker context: %w: %s", err, output)
		}
		if err := json.Unmarshal(bytes.TrimSpace(output), &endpoint); err != nil {
			return "", fmt.Errorf("decode current Docker context endpoint: %w", err)
		}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || !filepath.IsAbs(parsed.Path) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("integration requires a local Unix Docker endpoint, got %q", endpoint)
	}
	return "unix://" + filepath.Clean(parsed.Path), nil
}

func environmentWith(name, value string) []string {
	prefix := name + "="
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, prefix) {
			environment = append(environment, entry)
		}
	}
	return append(environment, prefix+value)
}

func assertTransport(t *testing.T, record state.Workspace, want string) {
	t.Helper()
	for _, resource := range record.Resources {
		if resource.Type == want && resource.ReleasedAt == nil && !resource.Quarantined {
			return
		}
	}
	t.Fatalf("active %s transport missing: %#v", want, record.Resources)
}

func activeResourcePath(t *testing.T, record state.Workspace, resourceType string) string {
	t.Helper()
	for _, resource := range record.Resources {
		if resource.Type == resourceType && resource.ReleasedAt == nil && !resource.Quarantined {
			if path := resource.Identity["path"]; path != "" {
				return path
			}
		}
	}
	t.Fatalf("active %s resource path is missing", resourceType)
	return ""
}

func assertResources(t *testing.T, hostConfig container.HostConfig, expected config.ResourceSpec) {
	t.Helper()
	resources := hostConfig.Resources
	if !expected.Enabled {
		if resources.NanoCPUs != 0 || resources.Memory != 0 || resources.MemorySwap != 0 || resources.PidsLimit != nil || len(resources.Ulimits) != 0 {
			t.Fatalf("default resource constraints were set: %#v", resources)
		}
		return
	}
	wantNanoCPUs := int64(expected.CPU * 1_000_000_000)
	if resources.NanoCPUs != wantNanoCPUs || resources.Memory != int64(expected.Memory) || resources.MemorySwap != int64(expected.MemorySwap) ||
		resources.PidsLimit == nil || *resources.PidsLimit != expected.PIDs || len(resources.Ulimits) != 1 ||
		resources.Ulimits[0].Name != "nofile" || resources.Ulimits[0].Soft != int64(expected.Nofile.Soft) || resources.Ulimits[0].Hard != int64(expected.Nofile.Hard) {
		t.Fatalf("resource constraints=%#v want=%#v", resources, expected)
	}
}

func assertFile(t *testing.T, path, expected string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != expected {
		t.Fatalf("%s=%q want=%q err=%v", path, raw, expected, err)
	}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
