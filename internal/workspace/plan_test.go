package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
)

func availableDocker() runtime.BackendInfo {
	return runtime.BackendInfo{Name: "docker", Available: true, Capabilities: map[runtime.Capability]bool{runtime.CapabilityHostSocketBind: true, runtime.CapabilityRuntimeSelect: true}}
}

func TestCompilePlanDefaultsAndResources(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", "/tmp/ssh.sock", availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != state.WorkspacePlanSchemaVersion || plan.Resources.Enabled || plan.SSH.Kind != "directory-uds" || plan.Mounts[0].ReadOnly || plan.RuntimeAlias != "" {
		t.Fatalf("default plan = %#v", plan)
	}
	workspace.Spec.Resources.Enabled = true
	workspace.Spec.Resources.CPU = 8
	workspace.Spec.Resources.Memory = 32 << 30
	workspace.Spec.Resources.MemorySwap = 64 << 30
	workspace.Spec.Resources.PIDs = 4096
	workspace.Spec.Resources.Nofile = config.NofileLimit{Soft: 65536, Hard: 65536}
	plan, err = CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", "/tmp/ssh.sock", availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Resources.Enabled || plan.Resources.NanoCPUs != 8_000_000_000 || plan.Resources.MemoryBytes != 32<<30 || plan.Resources.MemorySwapBytes != 64<<30 || plan.Resources.PIDs != 4096 || plan.SSH.Kind != "directory-uds" {
		t.Fatalf("resource plan = %#v", plan)
	}
}

func TestCompilePlanDoesNotCreateAbsentReservedPaths(t *testing.T) {
	source := t.TempDir()
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, source, "manifest", image, "", "/tmp/ssh.sock", availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".cohotfs", ".omp"} {
		if _, err := os.Lstat(filepath.Join(source, name)); !os.IsNotExist(err) {
			t.Fatalf("compile created reserved source path %s: %v", name, err)
		}
		target := filepath.Join("/workspace", name)
		for _, mounted := range plan.Mounts {
			if mounted.Target == target {
				t.Fatalf("absent reserved path received a mount: %#v", mounted)
			}
		}
	}
}

func TestCompilePlanMasksExistingReservedDirectories(t *testing.T) {
	source := t.TempDir()
	for _, name := range []string{".cohotfs", ".omp"} {
		if err := os.Mkdir(filepath.Join(source, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, source, "manifest", image, "", "/tmp/ssh.sock", availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".cohotfs", ".omp"} {
		target := filepath.Join("/workspace", name)
		found := false
		for _, mounted := range plan.Mounts {
			if mounted.Target == target {
				found = mounted.Type == "tmpfs" && mounted.Source == "" && mounted.ReadOnly
			}
		}
		if !found {
			t.Fatalf("existing reserved directory %s is not masked: %#v", name, plan.Mounts)
		}
	}
	if len(plan.ReservedMasks) != 2 {
		t.Fatalf("reserved mask identities = %#v", plan.ReservedMasks)
	}
	for _, identity := range plan.ReservedMasks {
		info, err := os.Lstat(filepath.Join(source, identity.Name))
		if err != nil {
			t.Fatal(err)
		}
		current, err := reservedWorkspaceMaskIdentity(identity.Name, info)
		if err != nil {
			t.Fatal(err)
		}
		if identity != current {
			t.Fatalf("persisted identity = %#v, current = %#v", identity, current)
		}
	}
}

func TestCompilePlanRejectsUnsafeReservedPath(t *testing.T) {
	source := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(source, ".omp")); err != nil {
		t.Fatal(err)
	}
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	if _, err := CompilePlan(workspace, "workspace", 1000, 1000, source, "manifest", image, "", "/tmp/ssh.sock", availableDocker()); err == nil || !strings.Contains(err.Error(), "non-symlink directory") {
		t.Fatalf("unsafe reserved path error = %v", err)
	}
}

func TestValidateReservedWorkspaceMasksRejectsSourceChanges(t *testing.T) {
	source := t.TempDir()
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, source, "manifest", image, "", "/tmp/ssh.sock", availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReservedWorkspaceMasks(plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, ".omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateReservedWorkspaceMasks(plan); err == nil || !strings.Contains(err.Error(), "changed after plan compilation") {
		t.Fatalf("new reserved path validation error = %v", err)
	}
	if err := os.Remove(filepath.Join(source, ".omp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, ".cohotfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err = CompilePlan(workspace, "workspace", 1000, 1000, source, "manifest", image, "", "/tmp/ssh.sock", availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".cohotfs", "host-state"), []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateReservedWorkspaceMasks(plan); err != nil {
		t.Fatalf("reserved directory content change invalidated identity: %v", err)
	}
	legacyPlan := plan
	legacyPlan.ReservedMasks = nil
	if err := validateReservedWorkspaceMasks(legacyPlan); err == nil || !strings.Contains(err.Error(), "identity is unavailable") {
		t.Fatalf("missing reserved identity validation error = %v", err)
	}
	if err := os.Remove(filepath.Join(source, ".cohotfs", "host-state")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, ".cohotfs")); err != nil {
		t.Fatal(err)
	}
	if err := validateReservedWorkspaceMasks(plan); err == nil || !strings.Contains(err.Error(), "changed after plan compilation") {
		t.Fatalf("removed reserved path validation error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(source, ".cohotfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateReservedWorkspaceMasks(plan); err == nil || !strings.Contains(err.Error(), "changed after plan compilation") {
		t.Fatalf("replaced reserved path validation error = %v", err)
	}
}

func TestCompilePlanRejectsAbsentDirectoryTransport(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	if _, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", "", availableDocker()); err == nil || !runtime.IsUnsupported(err) {
		t.Fatalf("missing directory transport error = %v", err)
	}
}

func TestCompilePlanGVisorFailClosed(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Runtime.Isolation = "gvisor"
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	if _, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", "/tmp/ssh.sock", availableDocker()); err == nil {
		t.Fatal("accepted gVisor without configured alias")
	}
	backend := availableDocker()
	delete(backend.Capabilities, runtime.CapabilityRuntimeSelect)
	if _, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "runsc", "/tmp/ssh.sock", backend); err == nil || !runtime.IsUnsupported(err) {
		t.Fatalf("missing runtime capability error = %v", err)
	}
}

func TestCompilePlanDetectsRegisteredGVisorAlias(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Runtime.Isolation = "gvisor"
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	backend := availableDocker()
	backend.Runtimes = []string{"runc", "runsc"}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", "/tmp/ssh.sock", backend)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RuntimeAlias != "runsc" {
		t.Fatalf("runtime alias = %q", plan.RuntimeAlias)
	}
}

func TestRuntimeSpecScopesGVisorHostUDSPolicy(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	workspace.Spec.Runtime.Isolation = "gvisor"
	workspace.Spec.Integrations.Agents.OMP.Enabled = true
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	backend := availableDocker()
	backend.Runtimes = []string{"runc", "runsc"}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", "/tmp/ssh.sock", backend)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.RuntimeSpec(runtime.Mount{}).GVisorHostUDS; got != runtime.GVisorHostUDSAll {
		t.Fatalf("gVisor agent integration host UDS policy = %q", got)
	}
	for _, integration := range []string{"browser", "sshAgent", "gitCredentials", "agent:omp", "agent:codex", "agent:claude"} {
		plan.Integrations[integration] = false
	}
	if got := plan.RuntimeSpec(runtime.Mount{}).GVisorHostUDS; got != runtime.GVisorHostUDSCreate {
		t.Fatalf("gVisor SSH-only host UDS policy = %q", got)
	}
	plan.RuntimeAlias = ""
	if got := plan.RuntimeSpec(runtime.Mount{}).GVisorHostUDS; got != "" {
		t.Fatalf("standard runtime host UDS policy = %q", got)
	}
}

func TestRuntimeSpecContainsIdentityLabelsAndBootstrap(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", filepath.Join(t.TempDir(), "ssh.sock"), availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := runtime.Mount{Source: "/tmp/bootstrap", Target: "/run/cohotfs/bootstrap", ReadOnly: true, Type: "bind", Propagation: "rprivate"}
	spec := plan.RuntimeSpec(bootstrap)
	if spec.Labels[LabelWorkspaceID] != plan.WorkspaceID || spec.Labels[LabelCreationNonce] != plan.CreationNonce || spec.Mounts[len(spec.Mounts)-1] != bootstrap {
		t.Fatalf("runtime spec = %#v", spec)
	}
}

func TestRuntimeSpecCombinesOMPAndGoToolchainPaths(t *testing.T) {
	workspace := config.BuiltinWorkspace("api", "example.invalid/base:dev")
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	plan, err := CompilePlan(workspace, "workspace", 1000, 1000, t.TempDir(), "manifest", image, "", filepath.Join(t.TempDir(), "ssh.sock"), availableDocker())
	if err != nil {
		t.Fatal(err)
	}
	plan.Toolchains.Environment = []string{
		"GOROOT=/cohotfs/toolchains/go/root",
		"PATH=/cohotfs/toolchains/go/state/bin:/cohotfs/toolchains/go/root/bin:/usr/local/bin:/usr/bin:/bin",
	}
	plan.OMP.Environment = []string{
		"PI_CODING_AGENT_DIR=/home/agent/.omp/agent",
		"PATH=/cohotfs/agents/omp/bin:/usr/local/bin:/usr/bin:/bin",
	}
	spec := plan.RuntimeSpec(runtime.Mount{})
	pathCount := 0
	path := ""
	for _, item := range spec.Environment {
		if strings.HasPrefix(item, "PATH=") {
			pathCount++
			path = item
		}
	}
	const expected = "PATH=/cohotfs/agents/omp/bin:/cohotfs/toolchains/go/state/bin:/cohotfs/toolchains/go/root/bin:/usr/local/bin:/usr/bin:/bin"
	if pathCount != 1 || path != expected {
		t.Fatalf("runtime PATH count=%d value=%q environment=%#v", pathCount, path, spec.Environment)
	}
}
