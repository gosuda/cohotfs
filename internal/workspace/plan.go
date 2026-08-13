package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/ompimport"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/toolchain"
)

type SSHTransport struct {
	Kind       string `json:"kind"`
	SocketPath string `json:"socketPath"`
}

type Plan struct {
	SchemaVersion       int                     `json:"schemaVersion"`
	WorkspaceID         string                  `json:"workspaceID"`
	Name                string                  `json:"name"`
	OwnerUID            int                     `json:"ownerUID"`
	OwnerGID            int                     `json:"ownerGID"`
	CanonicalSource     string                  `json:"canonicalSource"`
	ManifestDigest      string                  `json:"manifestDigest"`
	Backend             string                  `json:"backend"`
	CreationNonce       string                  `json:"creationNonce"`
	Image               runtime.ResolvedImage   `json:"image"`
	RuntimeAlias        string                  `json:"runtimeAlias,omitempty"`
	ContainerUID        int                     `json:"containerUID"`
	ContainerGID        int                     `json:"containerGID"`
	Mounts              []runtime.Mount         `json:"mounts"`
	Environment         []string                `json:"environment"`
	Resources           runtime.ResourceLimits  `json:"resources"`
	SSH                 SSHTransport            `json:"ssh"`
	Required            []runtime.Capability    `json:"requiredCapabilities"`
	Integrations        map[string]bool         `json:"integrations"`
	IntegrationSettings config.IntegrationsSpec `json:"integrationSettings"`
	Toolchains          toolchain.Plan          `json:"toolchains,omitempty"`
	OMP                 ompimport.Plan          `json:"omp,omitempty"`
	Setup               config.SetupSpec        `json:"setup"`
	CreatedAt           time.Time               `json:"createdAt"`
}

func CompilePlan(workspace config.Workspace, workspaceID string, ownerUID, ownerGID int, canonicalSource, manifestDigest string, image runtime.ResolvedImage, gvisorRuntime, sshSocketPath string, backend runtime.BackendInfo) (Plan, error) {
	if err := workspace.Validate(); err != nil {
		return Plan{}, err
	}
	if workspaceID == "" || ownerUID <= 0 || ownerGID <= 0 || !filepath.IsAbs(canonicalSource) || image.Digest == "" {
		return Plan{}, fmt.Errorf("workspace identity and resolved image are required")
	}
	if backend.Name != workspace.Spec.Runtime.Backend || !backend.Available {
		return Plan{}, fmt.Errorf("backend is unavailable")
	}
	if image.BootstrapAPI != containeragent.BootstrapAPI {
		return Plan{}, fmt.Errorf("image_incompatible: bootstrap API mismatch")
	}
	nonce, err := randomHex(16)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		SchemaVersion: 1, WorkspaceID: workspaceID, Name: workspace.Metadata.Name,
		OwnerUID: ownerUID, OwnerGID: ownerGID, CanonicalSource: canonicalSource,
		ManifestDigest: manifestDigest, Backend: workspace.Spec.Runtime.Backend, CreationNonce: nonce,
		Image: image, ContainerUID: ownerUID, ContainerGID: ownerGID, Setup: workspace.Spec.Setup,
		CreatedAt: time.Now().UTC(), Integrations: map[string]bool{
			"hostToolchains": workspace.Spec.Integrations.HostToolchains.Enabled,
			"browser":        workspace.Spec.Integrations.Browser.Enabled,
			"sshAgent":       workspace.Spec.Integrations.SSHAgent.Enabled,
			"gitCredentials": workspace.Spec.Integrations.GitCredentials.Enabled,
			"agent:omp":      workspace.Spec.Integrations.Agents.OMP.Enabled,
			"agent:codex":    workspace.Spec.Integrations.Agents.Codex.Enabled,
			"agent:claude":   workspace.Spec.Integrations.Agents.Claude.Enabled,
		},
		IntegrationSettings: workspace.Spec.Integrations,
	}
	plan.Mounts = append(plan.Mounts,
		runtime.Mount{Source: canonicalSource, Target: workspace.Spec.Workspace.Target, Type: "bind", Propagation: "rprivate"},
		runtime.Mount{Target: filepath.Join(workspace.Spec.Workspace.Target, ".cohotfs"), ReadOnly: true, Type: "tmpfs"},
		runtime.Mount{Target: filepath.Join(workspace.Spec.Workspace.Target, ".omp"), ReadOnly: true, Type: "tmpfs"},
	)
	plan.Environment = []string{"HOME=/home/agent", "XDG_CONFIG_HOME=/home/agent/.config", "XDG_CACHE_HOME=/home/agent/.cache", "XDG_DATA_HOME=/home/agent/.local/share", "TMPDIR=/home/agent/.tmp", "TERM=xterm-256color", "COLORTERM=truecolor"}
	if workspace.Spec.Integrations.Browser.Enabled {
		plan.Environment = append(plan.Environment, "COHOTFS_CDP_URL=http://127.0.0.1:9222")
	}
	if workspace.Spec.Resources.Enabled {
		cpu := workspace.Spec.Resources.CPU * 1_000_000_000
		if cpu > math.MaxInt64 {
			return Plan{}, fmt.Errorf("CPU value overflows Docker nano-CPU field")
		}
		plan.Resources = runtime.ResourceLimits{Enabled: true, NanoCPUs: int64(cpu), MemoryBytes: int64(workspace.Spec.Resources.Memory), MemorySwapBytes: int64(workspace.Spec.Resources.MemorySwap), PIDs: workspace.Spec.Resources.PIDs, NofileSoft: workspace.Spec.Resources.Nofile.Soft, NofileHard: workspace.Spec.Resources.Nofile.Hard}
	}
	if workspace.Spec.Runtime.Isolation == "gvisor" {
		plan.RuntimeAlias, err = resolveGVisorRuntimeAlias(gvisorRuntime, backend)
		if err != nil {
			return Plan{}, err
		}
		plan.Required = append(plan.Required, runtime.CapabilityRuntimeSelect)
	}
	if !backend.Capabilities[runtime.CapabilityHostSocketBind] || sshSocketPath == "" {
		return Plan{}, &runtime.UnsupportedError{Backend: backend.Name, Capability: runtime.CapabilityHostSocketBind, Reason: "directory-UDS SSH transport is required"}
	}
	plan.SSH = SSHTransport{Kind: "directory-uds", SocketPath: sshSocketPath}
	plan.Required = append(plan.Required, runtime.CapabilityHostSocketBind)
	plan.Mounts = append(plan.Mounts, runtime.Mount{Source: filepath.Dir(sshSocketPath), Target: "/run/cohotfs/transport/ssh", Type: "bind", Propagation: "rprivate"})
	for _, capability := range plan.Required {
		if !backend.Capabilities[capability] {
			return Plan{}, &runtime.UnsupportedError{Backend: backend.Name, Capability: capability}
		}
	}
	return plan, nil
}

func resolveGVisorRuntimeAlias(configured string, backend runtime.BackendInfo) (string, error) {
	if configured != "" {
		return configured, nil
	}
	for _, preferred := range []string{"runsc", "gvisor"} {
		for _, available := range backend.Runtimes {
			if available == preferred {
				return available, nil
			}
		}
	}
	for _, available := range backend.Runtimes {
		name := strings.ToLower(available)
		if strings.Contains(name, "runsc") || strings.Contains(name, "gvisor") {
			return available, nil
		}
	}
	return "", &runtime.UnsupportedError{
		Backend:    backend.Name,
		Capability: runtime.CapabilityRuntimeSelect,
		Reason:     "gVisor is not registered with Docker; configure runtime.docker.gvisorRuntime or install a runsc runtime",
	}
}

func (p Plan) RuntimeSpec(bootstrapMount runtime.Mount) runtime.WorkspaceSpec {
	labels := map[string]string{
		LabelOwnerUID: fmt.Sprint(p.OwnerUID), LabelWorkspaceID: p.WorkspaceID,
		LabelManifest: p.ManifestDigest, LabelCreationNonce: p.CreationNonce,
	}
	mounts := append([]runtime.Mount{}, p.Mounts...)
	mounts = append(mounts, p.Toolchains.Mounts...)
	mounts = append(mounts, p.OMP.Mounts...)
	mounts = append(mounts, bootstrapMount)
	environment := runtimeEnvironment(p.Environment, p.Toolchains.Environment, p.OMP.Environment)
	return runtime.WorkspaceSpec{
		WorkspaceID: p.WorkspaceID, OwnerUID: p.OwnerUID, OwnerGID: p.OwnerGID,
		ManifestDigest: p.ManifestDigest, CreationNonce: p.CreationNonce, Image: p.Image,
		Runtime: p.RuntimeAlias, Environment: environment, Mounts: mounts,
		Resources: p.Resources, Labels: labels,
	}
}

func runtimeEnvironment(base, toolchains, omp []string) []string {
	groups := [][]string{base, toolchains, omp}
	environment := make([]string, 0, len(base)+len(toolchains)+len(omp))
	for _, group := range groups {
		for _, item := range group {
			if !strings.HasPrefix(item, "PATH=") {
				environment = append(environment, item)
			}
		}
	}

	seen := make(map[string]bool)
	preferred := make([]string, 0, 8)
	system := make([]string, 0, 3)
	for _, group := range [][]string{omp, toolchains, base} {
		for _, item := range group {
			name, value, found := strings.Cut(item, "=")
			if !found || name != "PATH" {
				continue
			}
			for _, directory := range strings.Split(value, ":") {
				if directory == "" || seen[directory] {
					continue
				}
				seen[directory] = true
				switch directory {
				case "/usr/local/bin", "/usr/bin", "/bin":
					system = append(system, directory)
				default:
					preferred = append(preferred, directory)
				}
			}
		}
	}
	if len(preferred)+len(system) != 0 {
		preferred = append(preferred, system...)
		environment = append(environment, "PATH="+strings.Join(preferred, ":"))
	}
	return environment
}

func ManifestDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func randomHex(bytesCount int) (string, error) {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func RedactedPlan(plan Plan) ([]byte, error) {
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToLower(string(raw)), "token") || strings.Contains(strings.ToLower(string(raw)), "password") {
		return nil, fmt.Errorf("plan contains a secret-like field")
	}
	return json.MarshalIndent(plan, "", "  ")
}

func CanonicalSource(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("source must be a directory")
	}
	return canonical, nil
}
