package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/gosuda/cohotfs/internal/state"
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
	ReservedMasks       []ReservedWorkspaceMask `json:"reservedWorkspaceMasks,omitempty"`
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
		SchemaVersion: state.WorkspacePlanSchemaVersion, WorkspaceID: workspaceID, Name: workspace.Metadata.Name,
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
	plan.Mounts = append(plan.Mounts, runtime.Mount{
		Source: canonicalSource, Target: workspace.Spec.Workspace.Target, Type: "bind", Propagation: "rprivate",
	})
	reservedMounts, reservedMasks, err := reservedWorkspaceMaskMounts(canonicalSource, workspace.Spec.Workspace.Target)
	if err != nil {
		return Plan{}, err
	}
	plan.Mounts = append(plan.Mounts, reservedMounts...)
	plan.ReservedMasks = reservedMasks
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

type ReservedWorkspaceMask struct {
	Name   string `json:"name"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
}

func reservedWorkspaceMaskMounts(source, target string) ([]runtime.Mount, []ReservedWorkspaceMask, error) {
	mounts := make([]runtime.Mount, 0, 2)
	identities := make([]ReservedWorkspaceMask, 0, 2)
	for _, name := range []string{".cohotfs", ".omp"} {
		path := filepath.Join(source, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("inspect reserved workspace path %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, nil, fmt.Errorf("reserved workspace path %s must be a non-symlink directory", path)
		}
		identity, err := reservedWorkspaceMaskIdentity(name, info)
		if err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, runtime.Mount{
			Target: filepath.Join(target, name), ReadOnly: true, Type: "tmpfs",
		})
		identities = append(identities, identity)
	}
	return mounts, identities, nil
}

func validateReservedWorkspaceMasks(plan Plan) error {
	workspaceTarget := ""
	for _, mounted := range plan.Mounts {
		if mounted.Type == "bind" && mounted.Source == plan.CanonicalSource {
			workspaceTarget = mounted.Target
			break
		}
	}
	if workspaceTarget == "" {
		return fmt.Errorf("workspace source mount is unavailable")
	}
	identities := make(map[string]ReservedWorkspaceMask, len(plan.ReservedMasks))
	for _, identity := range plan.ReservedMasks {
		if identity.Name != ".cohotfs" && identity.Name != ".omp" {
			return fmt.Errorf("reserved workspace mask identity %q is invalid", identity.Name)
		}
		if _, duplicate := identities[identity.Name]; duplicate {
			return fmt.Errorf("reserved workspace mask identity %q is duplicated", identity.Name)
		}
		identities[identity.Name] = identity
	}
	for _, name := range []string{".cohotfs", ".omp"} {
		masked := false
		target := filepath.Join(workspaceTarget, name)
		for _, mounted := range plan.Mounts {
			if mounted.Target == target {
				masked = mounted.Type == "tmpfs" && mounted.Source == "" && mounted.ReadOnly
				if !masked {
					return fmt.Errorf("reserved workspace path %s has an invalid mask", name)
				}
				break
			}
		}
		expectedIdentity, hasIdentity := identities[name]
		if masked != hasIdentity {
			return fmt.Errorf("reserved workspace path %s mask identity is unavailable; recreate the workspace", name)
		}
		path := filepath.Join(plan.CanonicalSource, name)
		info, err := os.Lstat(path)
		present := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect reserved workspace path %s: %w", name, err)
		}
		if present && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return fmt.Errorf("reserved workspace path %s must remain a non-symlink directory", name)
		}
		if present != masked {
			return fmt.Errorf("reserved workspace path %s changed after plan compilation; recreate the workspace", name)
		}
		if !present {
			continue
		}
		currentIdentity, err := reservedWorkspaceMaskIdentity(name, info)
		if err != nil {
			return err
		}
		if currentIdentity != expectedIdentity {
			return fmt.Errorf("reserved workspace path %s changed after plan compilation; recreate the workspace", name)
		}
	}
	return nil
}

func pinReservedWorkspaceMasks(plan Plan) (func(), error) {
	if err := validateReservedWorkspaceMasks(plan); err != nil {
		return nil, err
	}
	pins := make([]*os.File, 0, len(plan.ReservedMasks))
	release := func() {
		for _, pin := range pins {
			_ = pin.Close()
		}
	}
	for _, expected := range plan.ReservedMasks {
		path := filepath.Join(plan.CanonicalSource, expected.Name)
		pin, err := openReservedWorkspaceMask(path)
		if err != nil {
			release()
			return nil, fmt.Errorf("pin reserved workspace path %s: %w", expected.Name, err)
		}
		info, err := pin.Stat()
		if err != nil {
			_ = pin.Close()
			release()
			return nil, fmt.Errorf("inspect pinned workspace path %s: %w", expected.Name, err)
		}
		current, err := reservedWorkspaceMaskIdentity(expected.Name, info)
		if err != nil || current != expected {
			_ = pin.Close()
			release()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("reserved workspace path %s changed after plan compilation; recreate the workspace", expected.Name)
		}
		pins = append(pins, pin)
	}
	return release, nil
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
		Runtime: p.RuntimeAlias, GVisorHostUDS: p.gvisorHostUDSPolicy(),
		Environment: environment, Mounts: mounts, Resources: p.Resources, Labels: labels,
	}
}

func (p Plan) gvisorHostUDSPolicy() string {
	if p.RuntimeAlias == "" {
		return ""
	}
	for _, integration := range []string{"browser", "sshAgent", "gitCredentials", "agent:omp", "agent:codex", "agent:claude"} {
		if p.Integrations[integration] {
			return runtime.GVisorHostUDSAll
		}
	}
	return runtime.GVisorHostUDSCreate
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
