package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime/docker"
	"github.com/gosuda/cohotfs/internal/state"
	workspaceservice "github.com/gosuda/cohotfs/internal/workspace"
	"github.com/spf13/cobra"
)

func testDependencies(path string) Dependencies {
	return Dependencies{OpenRoot: func() (*hostroot.Root, error) { return hostroot.OpenForTest(path) }}
}

func TestExactTopLevelCommandTree(t *testing.T) {
	command := NewRootCommand(testDependencies(filepath.Join(t.TempDir(), "root")))
	got := make([]string, 0, len(command.Commands()))
	for _, child := range command.Commands() {
		got = append(got, child.Name())
	}
	sort.Strings(got)
	want := []string{"agent", "config", "doctor", "exec", "host", "image", "init", "onboard", "port-forward", "runtime", "setup", "shell", "ssh-proxy", "workspace"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("top-level commands = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("top-level commands = %v, want %v", got, want)
		}
	}
	onboard, _, err := command.Find([]string{"onboard"})
	if err != nil || onboard.Flags().Lookup("non-interactive") == nil {
		t.Fatalf("onboard --non-interactive missing: %v", err)
	}
	doctor, _, err := command.Find([]string{"doctor"})
	if err != nil || doctor.Flags().Lookup("output") == nil {
		t.Fatalf("doctor --output missing: %v", err)
	}
}

func TestDefaultImagePullPolicySeparatesDevelopmentAndRelease(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "dev"
	if got := defaultImagePullPolicy(); got != config.ImagePullNever {
		t.Fatalf("development pull policy = %q", got)
	}
	Version = "v0.1.0"
	if got := defaultImagePullPolicy(); got != config.ImagePullAlways {
		t.Fatalf("release pull policy = %q", got)
	}
}

func TestInitWritesOnlyTrustedHomeProjectConfig(t *testing.T) {
	project := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "omp"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)
	rootPath := filepath.Join(t.TempDir(), "root")
	var stdout, stderr bytes.Buffer
	if code := Execute(context.Background(), []string{"init"}, &stdout, &stderr, testDependencies(rootPath)); code != 0 {
		t.Fatalf("init code %d, stderr %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(project, ".cohotfs")); !os.IsNotExist(err) {
		t.Fatalf("init wrote repository configuration: %v", err)
	}
	_, _, key, err := config.ProjectIdentity(project)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootPath, "projects", key, "workspace.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := config.DecodeProject(raw, project)
	if err != nil {
		t.Fatalf("generated invalid project config: %v", err)
	}
	if document.Workspace.Spec.Image.Ref != "ghcr.io/gosuda/cohotfs/workspace-base:dev" || document.Workspace.Spec.Image.PullPolicy != config.ImagePullNever {
		t.Fatalf("generated development image policy = %#v", document.Workspace.Spec.Image)
	}
	if document.Workspace.Spec.Integrations.Agents.OMP.Enabled {
		t.Fatal("available OMP silently enabled a host integration")
	}
	if _, err := os.Stat(filepath.Join(project, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("init touched .gitignore: %v", err)
	}
}

func TestWorkspaceListJSONStableFields(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	root, err := hostroot.OpenForTest(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := state.NewWorkspaceID()
	now := time.Unix(10, 0).UTC()
	if err := store.SaveWorkspace(state.Workspace{ID: id, Name: "api", Backend: "docker", Status: state.StatusStopped, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()

	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"workspace", "list", "--output", "json"}, &stdout, &stderr, testDependencies(rootPath))
	if code != 0 {
		t.Fatalf("list code %d, stderr %s", code, stderr.String())
	}
	var records []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &records); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	for _, field := range []string{"id", "name", "status", "backend", "imageDigest", "source", "createdAt", "updatedAt"} {
		if _, ok := records[0][field]; !ok {
			t.Fatalf("missing stable field %s in %v", field, records[0])
		}
	}
}

func TestWorkspaceStatusDefaultsToCurrentDirectoryAndSupportsExplicitFlag(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	root, err := hostroot.OpenForTest(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	currentSource, _, _, err := config.ProjectIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherSource, _, _, err := config.ProjectIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	currentID, _ := state.NewWorkspaceID()
	otherID, _ := state.NewWorkspaceID()
	now := time.Unix(10, 0).UTC()
	for _, record := range []state.Workspace{
		{ID: currentID, Name: "current", CanonicalSource: currentSource, Backend: "docker", Status: state.StatusStopped, CreatedAt: now, UpdatedAt: now},
		{ID: otherID, Name: "other", CanonicalSource: otherSource, Backend: "docker", Status: state.StatusStopped, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.SaveWorkspace(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(currentSource); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)

	assertStatus := func(args []string, wantID string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), args, &stdout, &stderr, testDependencies(rootPath))
		if code != apperr.ExitSuccess {
			t.Fatalf("%v code=%d stderr=%q", args, code, stderr.String())
		}
		var summary workspaceSummary
		if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
			t.Fatalf("decode %q: %v", stdout.String(), err)
		}
		if summary.ID != wantID {
			t.Fatalf("%v selected %s, want %s", args, summary.ID, wantID)
		}
	}
	assertStatus([]string{"workspace", "status", "--output", "json"}, currentID)
	assertStatus([]string{"workspace", "status", "--workspace", "other", "--output", "json"}, otherID)

	var stdout, stderr bytes.Buffer
	if code := Execute(context.Background(), []string{"workspace", "status", "other"}, &stdout, &stderr, testDependencies(rootPath)); code != apperr.ExitUsage {
		t.Fatalf("legacy positional workspace code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestWorkspaceTargetingCommandsExposeWorkspaceFlag(t *testing.T) {
	root := NewRootCommand(testDependencies(filepath.Join(t.TempDir(), "root")))
	for _, path := range [][]string{
		{"shell"}, {"exec"}, {"port-forward"},
		{"workspace", "status"}, {"workspace", "start"}, {"workspace", "stop"}, {"workspace", "restart"},
		{"workspace", "remove"}, {"workspace", "recover"}, {"workspace", "rotate-host-key"},
		{"setup", "run"}, {"agent", "run"},
	} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("find %v: %v", path, err)
		}
		if command.Flags().Lookup("workspace") == nil {
			t.Fatalf("%v has no --workspace flag", path)
		}
	}
}

func TestBareCommandHomeRequiresExplicitGrant(t *testing.T) {
	home := t.TempDir()
	if _, _, err := validateBareDirectory(home, home, false); apperr.Code(err) != apperr.ExitPolicy {
		t.Fatalf("home without grant error = %v, code = %d", err, apperr.Code(err))
	}
	canonical, homeMount, err := validateBareDirectory(home, home, true)
	if err != nil {
		t.Fatalf("home with explicit grant: %v", err)
	}
	if !homeMount {
		t.Fatal("explicit home mount was not identified for Cohotfs state masking")
	}
	alias := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateBareDirectory(alias, home, false); apperr.Code(err) != apperr.ExitPolicy {
		t.Fatalf("symlinked home without grant error = %v, code = %d", err, apperr.Code(err))
	}
	want, _, _, err := config.ProjectIdentity(home)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != want {
		t.Fatalf("canonical directory = %q, want %q", canonical, want)
	}
	command := NewRootCommand(testDependencies(filepath.Join(t.TempDir(), "root")))
	if command.Flags().Lookup("allow-home") == nil {
		t.Fatal("bare command has no --allow-home flag")
	}
}

func TestEnsureSSHClientKeyCreatesStableIdentity(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureSSHClientKey(root); err != nil {
		t.Fatal(err)
	}
	privateBefore, err := root.ReadFile("ssh/id_ed25519")
	if err != nil {
		t.Fatal(err)
	}
	publicBefore, err := root.ReadFile("ssh/id_ed25519.pub")
	if err != nil {
		t.Fatal(err)
	}
	privatePath, _ := root.HostPath("ssh/id_ed25519")
	publicPath, _ := root.HostPath("ssh/id_ed25519.pub")
	privateInfo, err := os.Lstat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	publicInfo, err := os.Lstat(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0o600 || publicInfo.Mode().Perm()&0o400 == 0 || publicInfo.Mode().Perm()&0o133 != 0 {
		t.Fatalf("unsafe SSH key modes = %o, %o", privateInfo.Mode().Perm(), publicInfo.Mode().Perm())
	}
	if err := ensureSSHClientKey(root); err != nil {
		t.Fatal(err)
	}
	privateAfter, _ := root.ReadFile("ssh/id_ed25519")
	publicAfter, _ := root.ReadFile("ssh/id_ed25519.pub")
	if !bytes.Equal(privateBefore, privateAfter) || !bytes.Equal(publicBefore, publicAfter) {
		t.Fatal("SSH client identity changed on repeated ensure")
	}
}

func TestPrepareBareWorkspaceMountsCurrentDirectoryWithoutManifest(t *testing.T) {
	project := filepath.Join(t.TempDir(), "Project With Spaces")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	prepared, err := prepareWorkspaceDirectory(root, project, config.BuiltinHostConfig(), true)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _, err := config.ProjectIdentity(project)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.source != canonical {
		t.Fatalf("mounted source = %q, want current directory %q", prepared.source, canonical)
	}
	if prepared.workspace.Metadata.Name != "project-with-spaces" {
		t.Fatalf("implicit workspace name = %q", prepared.workspace.Metadata.Name)
	}
	if prepared.workspace.Spec.Image.PullPolicy != config.ImagePullNever {
		t.Fatalf("implicit development image pull policy = %q", prepared.workspace.Spec.Image.PullPolicy)
	}
	if prepared.digest == "" {
		t.Fatal("implicit workspace digest is empty")
	}
	if _, err := os.Stat(filepath.Join(project, ".cohotfs", "workspace.yaml")); !os.IsNotExist(err) {
		t.Fatalf("bare preparation wrote a repository manifest: %v", err)
	}
}

func TestPrepareBareWorkspaceIgnoresRepositoryManifest(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".cohotfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := config.BuiltinWorkspace("configured", "example.invalid/base:dev")
	workspace.Spec.Workspace.Source = "child"
	workspace.Spec.Workspace.Target = "/configured-target"
	raw, err := config.Render(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".cohotfs", "workspace.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	prepared, err := prepareWorkspaceDirectory(root, project, config.BuiltinHostConfig(), true)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _, err := config.ProjectIdentity(project)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.source != canonical || prepared.workspace.Spec.Workspace.Source != "." || prepared.workspace.Spec.Workspace.Target != "/workspace" {
		t.Fatalf("bare workspace source contract = %#v, mounted %q", prepared.workspace.Spec.Workspace, prepared.source)
	}
	effective, err := config.Render(prepared.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.digest != workspaceservice.ManifestDigest(effective) {
		t.Fatal("bare workspace digest does not describe the effective mount contract")
	}
	if prepared.workspace.Metadata.Name == "configured" || prepared.digest == workspaceservice.ManifestDigest(raw) {
		t.Fatal("bare workspace consumed repository-local configuration")
	}
}

func TestBareWorkspaceRemoteCommandStartsInWorkspace(t *testing.T) {
	remote := bareWorkspaceRemoteCommand()
	if len(remote) != 3 || remote[0] != "/bin/bash" || remote[1] != "-lc" || remote[2] != "cd /workspace && exec /bin/bash -l" {
		t.Fatalf("bare remote command = %#v", remote)
	}
}

func TestWorkspaceForDirectoryRejectsChangedConfiguration(t *testing.T) {
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
	prepared := preparedWorkspace{source: t.TempDir(), digest: "new"}
	if err := store.SaveWorkspace(state.Workspace{ID: id, CanonicalSource: prepared.source, ManifestDigest: "old", Status: state.StatusReady}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspaceForDirectory(store, prepared); apperr.Code(err) != apperr.ExitStateConflict {
		t.Fatalf("changed configuration error = %v, code = %d", err, apperr.Code(err))
	}
}

func TestNonInteractiveOnboardAndDoctorDoNotMutateInjectedRoot(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"onboard", "--non-interactive"}, &stdout, &stderr, testDependencies(rootPath))
	if code != apperr.ExitSuccess && code != apperr.ExitUnavailable {
		t.Fatalf("non-interactive onboard code = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(rootPath); !os.IsNotExist(err) {
		t.Fatalf("non-interactive onboard created host root: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"doctor", "--output", "json"}, &stdout, &stderr, testDependencies(rootPath))
	if code != apperr.ExitSuccess && code != apperr.ExitUnavailable {
		t.Fatalf("doctor code = %d, stderr = %s", code, stderr.String())
	}
	var diagnostics diagnosticsOutput
	if err := json.Unmarshal(stdout.Bytes(), &diagnostics); err != nil {
		t.Fatalf("doctor JSON = %q: %v", stdout.String(), err)
	}
	if len(diagnostics.Checks) == 0 {
		t.Fatal("doctor returned no checks")
	}
	if _, err := os.Lstat(rootPath); !os.IsNotExist(err) {
		t.Fatalf("doctor created host root: %v", err)
	}
}

func TestInteractiveOnboardCreatesConfigAndSSHIdentity(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"onboard"}, &stdout, &stderr, testDependencies(rootPath))
	if code != apperr.ExitSuccess && code != apperr.ExitUnavailable {
		t.Fatalf("onboard code = %d, stderr = %s", code, stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(rootPath, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.DecodeHost(raw); err != nil {
		t.Fatalf("onboard config: %v", err)
	}
	privateInfo, err := os.Lstat(filepath.Join(rootPath, "ssh", "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("onboard private key mode = %o", privateInfo.Mode().Perm())
	}
}

func TestWorkspaceRemoveRequiresConfirmationBeforeRuntimeAccess(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"workspace", "remove", "--workspace", "example"}, &stdout, &stderr, testDependencies(rootPath))
	if code != apperr.ExitStateConflict {
		t.Fatalf("remove code = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Lstat(rootPath); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed removal touched host state: %v", err)
	}
}

func TestVersionFlagDoesNotOpenHostRoot(t *testing.T) {
	var stdout, stderr bytes.Buffer
	opened := false
	code := Execute(context.Background(), []string{"--version"}, &stdout, &stderr, Dependencies{OpenRoot: func() (*hostroot.Root, error) {
		opened = true
		return nil, errors.New("unexpected root access")
	}})
	if code != apperr.ExitSuccess || opened || !strings.Contains(stdout.String(), Version) {
		t.Fatalf("version code=%d opened=%v stdout=%q stderr=%q", code, opened, stdout.String(), stderr.String())
	}
}

func TestStableOperationKeyBindsInputAndHoldsLease(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	key, acknowledge, err := stableOperationKey(root, "workspace", "start", []byte("secret input"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := stableOperationKey(root, "workspace", "start", []byte("secret input")); err == nil || apperr.Code(err) != apperr.ExitStateConflict {
		t.Fatalf("concurrent operation key error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root.Path(), "run", "operations"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("operation key entries = %v, %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(root.Path(), "run", "operations", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret input") || !strings.Contains(string(raw), key) {
		t.Fatalf("operation key file leaked input or omitted key: %q", raw)
	}
	if err := acknowledge(); err != nil {
		t.Fatal(err)
	}

	retryKey, retryAcknowledge, err := stableOperationKey(root, "workspace", "start", []byte("changed input"))
	if err != nil {
		t.Fatal(err)
	}
	defer retryAcknowledge()
	if retryKey == key {
		t.Fatal("acknowledged operation reused its previous key")
	}
}

func TestWorkspaceRuntimeBlocksQuarantineExceptRecovery(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "root")
	root, err := hostroot.OpenForTest(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := state.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspace(state.Workspace{ID: id, Backend: "docker", Status: state.StatusError, Quarantined: true}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix:///tmp/cohotfs-test-docker.sock")

	called := false
	callback := func(*hostroot.Root, *state.Store, *docker.Adapter, *workspaceservice.DockerService, config.HostConfig) error {
		called = true
		return nil
	}
	err = withWorkspaceRuntime(context.Background(), testDependencies(rootPath), callback)
	if apperr.Code(err) != apperr.ExitPartialCleanup || called {
		t.Fatalf("ordinary operation error = %v, called = %v", err, called)
	}
	err = withWorkspaceRecoveryRuntime(context.Background(), testDependencies(rootPath), callback)
	if err != nil || !called {
		t.Fatalf("recovery operation error = %v, called = %v", err, called)
	}
}

func TestExternalProjectEditorCommitsOnlyValidatedConfiguration(t *testing.T) {
	project := t.TempDir()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	workspace := config.BuiltinWorkspace("project", "example.invalid/base:dev")
	path, err := writeProjectDocument(root, project, workspace)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalidEditor := filepath.Join(t.TempDir(), "invalid-editor")
	if err := os.WriteFile(invalidEditor, []byte("#!/bin/sh\nprintf 'invalid: [' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", invalidEditor)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetIn(bytes.NewReader(nil))
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := editProjectConfigExternally(command, root, path, project); err == nil {
		t.Fatal("accepted invalid edited project configuration")
	}
	afterInvalid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterInvalid, original) {
		t.Fatal("invalid editor result replaced the trusted configuration")
	}

	workspace.Spec.Integrations.Agents.OMP.Enabled = true
	document, err := config.NewProjectDocument(project, workspace)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := config.Render(document)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(t.TempDir(), "replacement.yaml")
	if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	validEditor := filepath.Join(t.TempDir(), "valid-editor")
	if err := os.WriteFile(validEditor, []byte("#!/bin/sh\nexec /bin/cp \"$REPLACEMENT\" \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPLACEMENT", replacementPath)
	t.Setenv("EDITOR", validEditor)
	if err := editProjectConfigExternally(command, root, path, project); err != nil {
		t.Fatal(err)
	}
	updated, err := config.LoadProject(path, project)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Workspace.Spec.Integrations.Agents.OMP.Enabled {
		t.Fatal("valid external edit was not committed")
	}
}

func TestSSHPTYModesDistinguishShellExecAndAgents(t *testing.T) {
	for _, test := range []struct {
		mode sshPTYMode
		want string
	}{
		{sshNoPTY, "-T"},
		{sshRequestPTY, "-t"},
		{sshForcePTY, "-tt"},
	} {
		got, err := appendSSHPTYArguments([]string{"base"}, test.mode)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[1] != test.want {
			t.Fatalf("PTY mode %d = %#v, want %s", test.mode, got, test.want)
		}
	}
	if _, err := appendSSHPTYArguments(nil, sshPTYMode(255)); err == nil {
		t.Fatal("accepted invalid SSH PTY mode")
	}
}

func TestProjectConfigTUITogglesAndSavesOMPGrant(t *testing.T) {
	model := projectConfigTUI{workspace: config.BuiltinWorkspace("project", "example.invalid/base:dev")}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	model = updated.(projectConfigTUI)
	if !model.workspace.Spec.Integrations.Agents.OMP.Enabled || model.saved {
		t.Fatalf("toggle result = %#v", model)
	}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model = updated.(projectConfigTUI)
	if !model.saved || command == nil || !model.workspace.Spec.Integrations.Agents.OMP.Import.Binary {
		t.Fatalf("save result = %#v, command nil=%v", model, command == nil)
	}
}
