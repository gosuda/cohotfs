package workspace

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/runtime"
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
	if plan.Resources.Enabled || plan.SSH.Kind != "directory-uds" || plan.Mounts[0].ReadOnly || plan.RuntimeAlias != "" {
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
