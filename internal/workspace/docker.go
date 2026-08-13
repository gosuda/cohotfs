package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/integration"
	"github.com/gosuda/cohotfs/internal/ompimport"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/runtime"
	setupservice "github.com/gosuda/cohotfs/internal/setup"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/gosuda/cohotfs/internal/toolchain"
)

type DockerService struct {
	root                   *hostroot.Root
	store                  *state.Store
	backend                runtime.Lifecycle
	now                    func() time.Time
	integrationHost        IntegrationHost
	integrationHostFactory IntegrationHostFactory
}

func NewDockerService(root *hostroot.Root, store *state.Store, backend runtime.Lifecycle) *DockerService {
	return &DockerService{root: root, store: store, backend: backend, now: time.Now}
}

type workspaceMutation func(state.Workspace) (state.Workspace, error)

func (s *DockerService) mutateWorkspace(ctx context.Context, id, key, operation string, body any, mutation workspaceMutation) (state.Workspace, error) {
	if key == "" {
		return state.Workspace{}, fmt.Errorf("idempotency key is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return state.Workspace{}, err
	}
	lock, err := s.store.LockWorkspace(id)
	if err != nil {
		return state.Workspace{}, err
	}
	defer lock.Close()
	journalOperation, replay, err := s.store.BeginOperation(id, key, operation, raw, s.now())
	if err != nil {
		return state.Workspace{}, err
	}
	if replay {
		var result state.Workspace
		if len(journalOperation.Result) != 0 {
			if err := json.Unmarshal(journalOperation.Result, &result); err != nil {
				return state.Workspace{}, err
			}
		}
		if journalOperation.Status == state.OperationFailed {
			return result, fmt.Errorf("previous %s operation failed: %s", operation, journalOperation.Error)
		}
		return result, nil
	}
	record, err := s.store.LoadWorkspace(id)
	if err != nil {
		_ = s.store.FinishOperation(id, key, nil, err, s.now())
		return state.Workspace{}, err
	}
	result, mutationErr := mutation(record)
	finishErr := s.store.FinishOperation(id, key, result, mutationErr, s.now())
	if mutationErr != nil {
		return result, errors.Join(mutationErr, finishErr)
	}
	return result, finishErr
}

type CreateRequest struct {
	Workspace           config.Workspace
	CanonicalSource     string
	ManifestDigest      string
	OwnerUID            int
	OwnerGID            int
	Image               runtime.ResolvedImage
	BackendInfo         runtime.BackendInfo
	GVisorRuntime       string
	SSHSocketPath       string
	BootstrapSource     string
	OperationKey        string
	ToolchainCandidates []toolchain.Candidate
	PermittedRoots      []string
	OMPSources          ompimport.Sources
	Environment         []string
	OverlayAvailable    bool
	MaskCohotfsRoot     bool
}

type createOperationInput struct {
	Workspace            config.Workspace            `json:"workspace"`
	CanonicalSource      string                      `json:"canonicalSource"`
	ManifestDigest       string                      `json:"manifestDigest"`
	OwnerUID             int                         `json:"ownerUID"`
	OwnerGID             int                         `json:"ownerGID"`
	ImageReference       string                      `json:"imageReference"`
	ImageDigest          string                      `json:"imageDigest"`
	BootstrapAPI         string                      `json:"bootstrapAPI"`
	Backend              string                      `json:"backend"`
	BackendCapabilities  map[runtime.Capability]bool `json:"backendCapabilities"`
	GVisorRuntime        string                      `json:"gvisorRuntime,omitempty"`
	SSHSocketPath        string                      `json:"sshSocketPath,omitempty"`
	AuthorizedKeySHA256  string                      `json:"authorizedKeySHA256"`
	ToolchainCandidates  []toolchain.Candidate       `json:"toolchainCandidates,omitempty"`
	PermittedRoots       []string                    `json:"permittedRoots,omitempty"`
	AgentSeedEnvironment map[string]string           `json:"agentSeedEnvironment,omitempty"`
	OMPSources           ompimport.Sources           `json:"ompSources,omitempty"`
	OverlayAvailable     bool                        `json:"overlayAvailable"`
	MaskCohotfsRoot      bool                        `json:"maskCohotfsRoot"`
}

func createOperationBody(request CreateRequest) ([]byte, error) {
	publicKey, err := os.ReadFile(request.BootstrapSource)
	if err != nil {
		return nil, fmt.Errorf("read authorized public key: %w", err)
	}
	permittedRoots := append([]string(nil), request.PermittedRoots...)
	sort.Strings(permittedRoots)
	seedEnvironment := make(map[string]string)
	allowedEnvironment := map[string]bool{
		"HOME": true, "XDG_CONFIG_HOME": true, "PI_CONFIG_DIR": true,
		"OMP_PROFILE": true, "CODEX_HOME": true, "CLAUDE_CONFIG_DIR": true,
	}
	for _, entry := range request.Environment {
		name, value, found := strings.Cut(entry, "=")
		if found && allowedEnvironment[name] {
			seedEnvironment[name] = value
		}
	}
	if len(seedEnvironment) == 0 {
		seedEnvironment = nil
	}
	return json.Marshal(createOperationInput{
		Workspace: request.Workspace, CanonicalSource: request.CanonicalSource, ManifestDigest: request.ManifestDigest,
		OwnerUID: request.OwnerUID, OwnerGID: request.OwnerGID,
		ImageReference: request.Image.Reference, ImageDigest: request.Image.Digest, BootstrapAPI: request.Image.BootstrapAPI,
		Backend: request.BackendInfo.Name, BackendCapabilities: request.BackendInfo.Capabilities,
		GVisorRuntime: request.GVisorRuntime, SSHSocketPath: request.SSHSocketPath,
		AuthorizedKeySHA256: fmt.Sprintf("%x", sha256.Sum256(publicKey)),
		ToolchainCandidates: request.ToolchainCandidates, PermittedRoots: permittedRoots, AgentSeedEnvironment: seedEnvironment,
		OMPSources: request.OMPSources, OverlayAvailable: request.OverlayAvailable, MaskCohotfsRoot: request.MaskCohotfsRoot,
	})
}

func (s *DockerService) Create(ctx context.Context, request CreateRequest) (result state.Workspace, returnErr error) {
	id, err := state.WorkspaceIDForOperationKey(request.OperationKey)
	if err != nil {
		return state.Workspace{}, err
	}
	lock, err := s.store.LockWorkspace(id)
	if err != nil {
		return state.Workspace{}, err
	}
	defer lock.Close()
	body, err := createOperationBody(request)
	if err != nil {
		return state.Workspace{}, err
	}
	journalOperation, replay, err := s.store.BeginOperation(id, request.OperationKey, "workspace.create", body, s.now())
	if err != nil {
		return state.Workspace{}, err
	}
	if replay {
		if len(journalOperation.Result) != 0 {
			if err := json.Unmarshal(journalOperation.Result, &result); err != nil {
				return state.Workspace{}, err
			}
		}
		if journalOperation.Status == state.OperationFailed {
			return result, fmt.Errorf("previous workspace.create operation failed: %s", journalOperation.Error)
		}
		return result, nil
	}
	operationRunning := true
	defer func() {
		if operationRunning {
			returnErr = errors.Join(returnErr, s.store.FinishOperation(id, request.OperationKey, result, returnErr, s.now()))
		}
	}()
	newWorkspace := false
	if existing, loadErr := s.store.LoadWorkspace(id); loadErr == nil {
		if existing.Status == state.StatusStopped {
			return existing, nil
		}
		if existing.Status != state.StatusCreating {
			return existing, fmt.Errorf("interrupted workspace creation is in state %s; recovery required", existing.Status)
		}
		resumed, complete, resumeErr := s.resumeCreatingLocked(ctx, request, existing)
		if resumeErr != nil || complete {
			return resumed, resumeErr
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return state.Workspace{}, loadErr
	} else {
		newWorkspace = true
	}
	cleanupPreparedState := newWorkspace
	defer func() {
		if cleanupPreparedState {
			returnErr = errors.Join(
				returnErr,
				s.root.RemoveTree(filepath.Join("run", "workspaces", id)),
				s.root.RemoveTree(filepath.Join("workspaces", id)),
			)
		}
	}()
	plan, err := CompilePlan(
		request.Workspace,
		id,
		request.OwnerUID,
		request.OwnerGID,
		request.CanonicalSource,
		request.ManifestDigest,
		request.Image,
		request.GVisorRuntime,
		request.SSHSocketPath,
		request.BackendInfo,
	)
	if err != nil {
		return state.Workspace{}, err
	}
	persistentMounts, err := s.preparePersistentStorage(id)
	if err != nil {
		return state.Workspace{}, err
	}
	plan.Mounts = append(plan.Mounts, persistentMounts...)
	if request.MaskCohotfsRoot {
		// Kept in the operation identity for clean replay of legacy callers.
	}
	plan.Toolchains, err = toolchain.Compile(s.root, id, request.Workspace.Spec.Integrations.HostToolchains, request.ToolchainCandidates, request.PermittedRoots, request.OverlayAvailable)
	if err != nil {
		return state.Workspace{}, err
	}
	plan.OMP, err = ompimport.Compile(s.root, id, request.Workspace.Spec.Integrations.Agents.OMP, request.OMPSources)
	if err != nil {
		return state.Workspace{}, err
	}
	bootstrapMount, err := s.prepareBootstrap(id, request.BootstrapSource, &plan, request.Environment)
	if err != nil {
		return state.Workspace{}, err
	}
	planRaw, err := RedactedPlan(plan)
	if err != nil {
		return state.Workspace{}, err
	}
	now := s.now().UTC()
	record := state.Workspace{
		ID: id, Name: plan.Name, OwnerUID: plan.OwnerUID, OwnerGID: plan.OwnerGID,
		CanonicalSource: plan.CanonicalSource, ManifestDigest: plan.ManifestDigest, Backend: plan.Backend,
		Capabilities: request.BackendInfo.Capabilities, ImageDigest: plan.Image.Digest, BootstrapAPI: plan.Image.BootstrapAPI,
		ContainerUID: plan.ContainerUID, ContainerGID: plan.ContainerGID, IntegrationGrants: plan.Integrations,
		Status: state.StatusCreating, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.SaveWorkspace(record); err != nil {
		return state.Workspace{}, err
	}
	cleanupPreparedState = false
	mountResources, err := toolchain.Activate(&plan.Toolchains, request.Workspace.Spec.Integrations.HostToolchains.RequireCOW)
	if err != nil {
		return s.quarantineCreate(record, fmt.Errorf("activate toolchain resources: %w", err))
	}
	record.Resources = append(record.Resources, mountResources...)
	if err := s.store.SaveWorkspace(record); err != nil {
		cleanupErr := toolchain.Deactivate(mountResources)
		if cleanupErr != nil {
			return s.quarantineCreate(record, errors.Join(err, cleanupErr))
		}
		return record, err
	}
	planRaw, err = RedactedPlan(plan)
	if err != nil {
		return s.failCreate(record, mountResources, err)
	}
	if err := s.store.SaveWorkspaceArtifact(id, "plan.json", append(planRaw, '\n')); err != nil {
		return s.failCreate(record, mountResources, err)
	}
	return s.createRuntimeLocked(ctx, record, plan, bootstrapMount)
}

func (s *DockerService) resumeCreatingLocked(ctx context.Context, request CreateRequest, record state.Workspace) (state.Workspace, bool, error) {
	if err := validateCreatingRecord(request, record); err != nil {
		quarantined, quarantineErr := s.quarantineCreate(record, err)
		return quarantined, true, quarantineErr
	}
	hasRuntime := record.RuntimeRef.Backend != "" || len(record.RuntimeRef.IDs) != 0
	if hasRuntime && (record.RuntimeRef.Backend != record.Backend || record.RuntimeRef.Nonce == "" || len(record.RuntimeRef.IDs) == 0) {
		quarantined, err := s.quarantineCreate(record, fmt.Errorf("persisted runtime reference identity is incomplete"))
		return quarantined, true, err
	}

	raw, err := s.store.LoadWorkspaceArtifact(record.ID, "plan.json")
	if errors.Is(err, os.ErrNotExist) {
		if hasRuntime {
			quarantined, quarantineErr := s.quarantineCreate(record, fmt.Errorf("persisted creation plan is missing for an allocated runtime object"))
			return quarantined, true, quarantineErr
		}
		active := activeMountResources(record.Resources)
		if cleanupErr := toolchain.Deactivate(active); cleanupErr != nil {
			quarantined, quarantineErr := s.quarantineCreate(record, cleanupErr)
			return quarantined, true, quarantineErr
		}
		if len(active) != 0 {
			released := s.now().UTC()
			for index := range record.Resources {
				if record.Resources[index].Type == "mount" && record.Resources[index].ReleasedAt == nil {
					record.Resources[index].ReleasedAt = &released
				}
			}
			if err := s.store.SaveWorkspace(record); err != nil {
				return record, true, err
			}
		}
		return record, false, nil
	}
	if err != nil {
		return record, true, err
	}
	var plan Plan
	if err := json.Unmarshal(raw, &plan); err != nil || !creatingPlanMatches(request, record, plan) {
		if err == nil {
			err = fmt.Errorf("persisted creation plan identity does not match the operation")
		}
		quarantined, quarantineErr := s.quarantineCreate(record, err)
		return quarantined, true, quarantineErr
	}
	if err := s.validateCreatingBootstrap(request, record, plan); err != nil {
		quarantined, quarantineErr := s.quarantineCreate(record, err)
		return quarantined, true, quarantineErr
	}
	active := activeMountResources(record.Resources)
	expectedOverlays := len(plan.Toolchains.Overlays)
	if len(active) != expectedOverlays {
		if len(active) != 0 {
			quarantined, quarantineErr := s.quarantineCreate(record, fmt.Errorf("persisted COW mount count does not match the creation plan"))
			return quarantined, true, quarantineErr
		}
		activated, activateErr := toolchain.Activate(&plan.Toolchains, request.Workspace.Spec.Integrations.HostToolchains.RequireCOW)
		if activateErr != nil {
			cleanupErr := toolchain.Deactivate(activated)
			quarantined, quarantineErr := s.quarantineCreate(record, errors.Join(fmt.Errorf("resume COW resources: %w", activateErr), cleanupErr))
			return quarantined, true, quarantineErr
		}
		record.Resources = append(record.Resources, activated...)
		if err := s.store.SaveWorkspace(record); err != nil {
			cleanupErr := toolchain.Deactivate(activated)
			if cleanupErr != nil {
				quarantined, quarantineErr := s.quarantineCreate(record, errors.Join(err, cleanupErr))
				return quarantined, true, quarantineErr
			}
			return record, true, err
		}
		planRaw, err := RedactedPlan(plan)
		if err != nil {
			failed, failErr := s.failCreate(record, activated, err)
			return failed, true, failErr
		}
		if err := s.store.SaveWorkspaceArtifact(record.ID, "plan.json", append(planRaw, '\n')); err != nil {
			failed, failErr := s.failCreate(record, activated, err)
			return failed, true, failErr
		}
		active = activated
	}
	for _, resource := range active {
		if resource.Quarantined {
			quarantined, quarantineErr := s.quarantineCreate(record, fmt.Errorf("persisted toolchain mount is quarantined"))
			return quarantined, true, quarantineErr
		}
	}
	if err := toolchain.ValidateResources(active); err != nil {
		quarantined, quarantineErr := s.quarantineCreate(record, err)
		return quarantined, true, quarantineErr
	}
	if hasRuntime {
		status, err := s.backend.Inspect(ctx, record.RuntimeRef)
		if err != nil {
			return record, true, apperr.Wrap(apperr.ExitRuntime, "runtime", err, "inspect interrupted Docker creation: %v", err)
		}
		if status.Exists {
			if err := validateRuntimeIdentity(record, status); err != nil {
				quarantined, quarantineErr := s.quarantineCreate(record, err)
				return quarantined, true, quarantineErr
			}
			if status.Running {
				quarantined, quarantineErr := s.quarantineCreate(record, fmt.Errorf("container from interrupted creation is unexpectedly running"))
				return quarantined, true, quarantineErr
			}
			if err := record.Transition(state.StatusStopped, s.now()); err != nil {
				return record, true, err
			}
			if err := s.store.SaveWorkspace(record); err != nil {
				return record, true, err
			}
			return record, true, nil
		}
		record.RuntimeRef = runtime.WorkspaceRef{}
		if err := s.store.SaveWorkspace(record); err != nil {
			return record, true, err
		}
	}
	bootstrapPath, err := s.root.HostPath(filepath.Join("workspaces", record.ID, "bootstrap"))
	if err != nil {
		return record, true, err
	}
	bootstrapMount := runtime.Mount{Source: bootstrapPath, Target: "/run/cohotfs/bootstrap", ReadOnly: true, Type: "bind", Propagation: "rprivate"}
	resumed, resumeErr := s.createRuntimeLocked(ctx, record, plan, bootstrapMount)
	return resumed, true, resumeErr
}

func validateCreatingRecord(request CreateRequest, record state.Workspace) error {
	if record.ID == "" || record.Name != request.Workspace.Metadata.Name ||
		record.OwnerUID != request.OwnerUID || record.OwnerGID != request.OwnerGID ||
		record.CanonicalSource != request.CanonicalSource || record.ManifestDigest != request.ManifestDigest ||
		record.Backend != request.BackendInfo.Name || record.ImageDigest != request.Image.Digest ||
		record.BootstrapAPI != request.Image.BootstrapAPI {
		return fmt.Errorf("persisted creating workspace identity does not match the operation")
	}
	return nil
}

func (s *DockerService) validateCreatingBootstrap(request CreateRequest, record state.Workspace, plan Plan) error {
	sourceKey, err := os.ReadFile(request.BootstrapSource)
	if err != nil {
		return fmt.Errorf("read authorized public key: %w", err)
	}
	persistedKey, err := s.root.ReadFile(filepath.Join("workspaces", record.ID, "bootstrap", "authorized_keys"))
	if err != nil {
		return fmt.Errorf("read persisted authorized key: %w", err)
	}
	if string(persistedKey) != string(sourceKey) {
		return fmt.Errorf("persisted authorized key does not match the creation operation")
	}
	raw, err := s.root.ReadFile(filepath.Join("workspaces", record.ID, "bootstrap", "bootstrap.json"))
	if err != nil {
		return fmt.Errorf("read persisted bootstrap contract: %w", err)
	}
	bootstrap, err := containeragent.ParseBootstrap(raw)
	if err != nil {
		return fmt.Errorf("decode persisted bootstrap contract: %w", err)
	}
	if bootstrap.BootstrapAPI != containeragent.BootstrapAPI || bootstrap.WorkspaceID != record.ID ||
		bootstrap.OwnerUID != record.OwnerUID || bootstrap.OwnerGID != record.OwnerGID ||
		bootstrap.AuthorizedKeyPath != "/run/cohotfs/bootstrap/authorized_keys" ||
		bootstrap.AllowAgentForwarding != plan.Integrations["sshAgent"] ||
		bootstrap.EnableCDP != plan.Integrations["browser"] ||
		bootstrap.EnableGitCredentials != plan.Integrations["gitCredentials"] ||
		bootstrap.EnableAgentSecrets != anyAgentEnabled(plan.IntegrationSettings.Agents) {
		return fmt.Errorf("persisted bootstrap contract does not match the creation plan")
	}
	if _, err := s.root.ReadFile(filepath.Join("workspaces", record.ID, "bootstrap", "seeds.json")); err != nil {
		return fmt.Errorf("read persisted agent seed manifest: %w", err)
	}
	return nil
}

func creatingPlanMatches(request CreateRequest, record state.Workspace, plan Plan) bool {
	return plan.SchemaVersion == state.WorkspacePlanSchemaVersion && plan.WorkspaceID == record.ID && plan.Name == record.Name &&
		plan.OwnerUID == record.OwnerUID && plan.OwnerGID == record.OwnerGID &&
		plan.CanonicalSource == record.CanonicalSource && plan.ManifestDigest == record.ManifestDigest &&
		plan.Backend == record.Backend && plan.Image.Digest == record.ImageDigest &&
		plan.Image.BootstrapAPI == record.BootstrapAPI && request.Image.BootstrapAPI == plan.Image.BootstrapAPI &&
		plan.CreationNonce != "" && plan.ContainerUID == record.ContainerUID && plan.ContainerGID == record.ContainerGID &&
		request.Image.Digest == plan.Image.Digest
}

func activeMountResources(resources []state.ExternalResource) []state.ExternalResource {
	active := make([]state.ExternalResource, 0, len(resources))
	for _, resource := range resources {
		if resource.Type == "mount" && resource.ReleasedAt == nil {
			active = append(active, resource)
		}
	}
	return active
}

func (s *DockerService) createRuntimeLocked(ctx context.Context, record state.Workspace, plan Plan, bootstrapMount runtime.Mount) (state.Workspace, error) {
	releasePins, err := pinReservedWorkspaceMasks(plan)
	if err != nil {
		return s.failCreate(record, activeMountResources(record.Resources), err)
	}
	defer func() { releasePins() }()
	runtimeSpec := plan.RuntimeSpec(bootstrapMount)
	runtimeSpec.Record = func(ref runtime.WorkspaceRef) error {
		record.RuntimeRef = ref
		return s.store.SaveWorkspace(record)
	}
	ref, err := s.backend.Create(ctx, runtimeSpec)
	if err != nil {
		if record.RuntimeRef.Backend != "" && len(record.RuntimeRef.IDs) != 0 {
			deleteErr := s.backend.Delete(context.WithoutCancel(ctx), record.RuntimeRef)
			if deleteErr != nil {
				return s.quarantineCreate(record, errors.Join(err, deleteErr))
			}
		}
		return s.failCreate(record, activeMountResources(record.Resources), err)
	}
	record.RuntimeRef = ref
	if err := validateReservedWorkspaceMasks(plan); err != nil {
		deleteErr := s.backend.Delete(context.WithoutCancel(ctx), ref)
		if deleteErr != nil {
			return s.quarantineCreate(record, errors.Join(err, deleteErr))
		}
		record.RuntimeRef = runtime.WorkspaceRef{}
		return s.failCreate(record, activeMountResources(record.Resources), err)
	}
	releasePins()
	releasePins = func() {}
	if err := s.store.SaveWorkspace(record); err != nil {
		deleteErr := s.backend.Delete(context.WithoutCancel(ctx), ref)
		if deleteErr != nil {
			return s.quarantineCreate(record, errors.Join(err, deleteErr))
		}
		return s.failCreate(record, activeMountResources(record.Resources), err)
	}
	if err := record.Transition(state.StatusStopped, s.now()); err != nil {
		return record, err
	}
	if err := s.store.SaveWorkspace(record); err != nil {
		return record, err
	}
	return record, nil
}

func (s *DockerService) quarantineCreate(record state.Workspace, cause error) (state.Workspace, error) {
	record.Quarantined = true
	_ = record.Transition(state.StatusError, s.now())
	saveErr := s.store.SaveWorkspace(record)
	joined := errors.Join(cause, saveErr)
	return record, apperr.Wrap(apperr.ExitPartialCleanup, "quarantined", joined, "workspace creation requires recovery: %v", joined)
}

func (s *DockerService) failCreate(record state.Workspace, mounts []state.ExternalResource, cause error) (state.Workspace, error) {
	cleanupErr := toolchain.Deactivate(mounts)
	if cleanupErr != nil {
		record.Quarantined = true
		for index := range record.Resources {
			if record.Resources[index].Type == "mount" && record.Resources[index].ReleasedAt == nil {
				record.Resources[index].Quarantined = true
			}
		}
		_ = record.Transition(state.StatusError, s.now())
		saveErr := s.store.SaveWorkspace(record)
		joined := errors.Join(cause, cleanupErr, saveErr)
		return record, apperr.Wrap(apperr.ExitPartialCleanup, "quarantined", joined, "workspace creation cleanup requires recovery: %v", joined)
	}
	released := s.now().UTC()
	for index := range record.Resources {
		if record.Resources[index].Type == "mount" && record.Resources[index].ReleasedAt == nil {
			record.Resources[index].ReleasedAt = &released
		}
	}
	transitionErr := record.Transition(state.StatusError, s.now())
	saveErr := s.store.SaveWorkspace(record)
	return record, errors.Join(cause, transitionErr, saveErr)
}

func (s *DockerService) preparePersistentStorage(id string) ([]runtime.Mount, error) {
	base := filepath.Join("workspaces", id)
	if err := s.root.EnsureDir(base, 0o700); err != nil {
		return nil, err
	}
	home := filepath.Join(base, "home")
	if err := s.root.EnsureDir(home, 0o700); err != nil {
		return nil, err
	}
	system := filepath.Join(base, "system")
	if err := s.root.EnsureDir(system, 0o700); err != nil {
		return nil, err
	}
	homePath, err := s.root.HostPath(home)
	if err != nil {
		return nil, err
	}
	systemPath, err := s.root.HostPath(system)
	if err != nil {
		return nil, err
	}
	return []runtime.Mount{
		{Source: homePath, Target: "/home/agent", Type: "bind", Propagation: "rprivate"},
		{Source: systemPath, Target: "/var/lib/cohotfs/system", Type: "bind", Propagation: "rprivate"},
	}, nil
}

func (s *DockerService) prepareBootstrap(id, publicKeySource string, plan *Plan, environment []string) (runtime.Mount, error) {
	if publicKeySource == "" {
		return runtime.Mount{}, fmt.Errorf("authorized public key is required")
	}
	bootstrapDir := "workspaces/" + id + "/bootstrap"
	if err := s.root.EnsureDir("workspaces/"+id, 0o700); err != nil {
		return runtime.Mount{}, err
	}
	if err := s.root.EnsureDir(bootstrapDir, 0o700); err != nil {
		return runtime.Mount{}, err
	}
	publicKey, err := os.ReadFile(publicKeySource)
	if err != nil {
		return runtime.Mount{}, err
	}
	if err := s.root.AtomicWrite(bootstrapDir+"/authorized_keys", publicKey, 0o644); err != nil {
		return runtime.Mount{}, err
	}
	if plan.Integrations["browser"] || plan.Integrations["gitCredentials"] || anyAgentEnabled(plan.IntegrationSettings.Agents) {
		integrationDir := filepath.Join("run", "workspaces", id)
		if err := s.root.EnsureDir(integrationDir, 0o700); err != nil {
			return runtime.Mount{}, err
		}
		hostPath, _ := s.root.HostPath(integrationDir)
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: hostPath, Target: "/run/cohotfs/integrations", ReadOnly: true, Type: "bind", Propagation: "rprivate"})
	}
	seedFiles, err := integration.StageAgentSeeds(s.root, id, requestEnvironmentAgents(plan), environment)
	if err != nil {
		return runtime.Mount{}, err
	}
	seedManifest := struct {
		SchemaVersion int                     `json:"schemaVersion"`
		Seeds         []containerSeedManifest `json:"seeds"`
	}{SchemaVersion: 1}
	for _, seed := range seedFiles {
		relative, err := filepath.Rel(filepath.Join(s.root.Path(), "workspaces", id, "agent-seeds"), seed.Source)
		if err != nil {
			return runtime.Mount{}, err
		}
		seedManifest.Seeds = append(seedManifest.Seeds, containerSeedManifest{Source: relative, Destination: seed.Destination, Mode: seed.Mode})
	}
	if len(seedFiles) != 0 {
		seedRoot, _ := s.root.HostPath(filepath.Join("workspaces", id, "agent-seeds"))
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: seedRoot, Target: "/run/cohotfs/agent-seeds", ReadOnly: true, Type: "bind", Propagation: "rprivate"})
	}
	seedsRaw, err := json.MarshalIndent(seedManifest, "", "  ")
	if err != nil {
		return runtime.Mount{}, err
	}
	if err := s.root.AtomicWrite(bootstrapDir+"/seeds.json", append(seedsRaw, '\n'), 0o600); err != nil {
		return runtime.Mount{}, err
	}
	bootstrap := struct {
		BootstrapAPI         string `json:"bootstrapAPI"`
		WorkspaceID          string `json:"workspaceID"`
		OwnerUID             int    `json:"ownerUID"`
		OwnerGID             int    `json:"ownerGID"`
		AuthorizedKeyPath    string `json:"authorizedKeyPath"`
		AllowAgentForwarding bool   `json:"allowAgentForwarding"`
		EnableCDP            bool   `json:"enableCDP"`
		EnableGitCredentials bool   `json:"enableGitCredentials"`
		EnableAgentSecrets   bool   `json:"enableAgentSecrets"`
	}{
		BootstrapAPI: containeragent.BootstrapAPI, WorkspaceID: id, OwnerUID: plan.OwnerUID, OwnerGID: plan.OwnerGID,
		AuthorizedKeyPath: "/run/cohotfs/bootstrap/authorized_keys", AllowAgentForwarding: plan.Integrations["sshAgent"],
		EnableCDP:            plan.Integrations["browser"],
		EnableGitCredentials: plan.Integrations["gitCredentials"], EnableAgentSecrets: anyAgentEnabled(plan.IntegrationSettings.Agents),
	}
	raw, err := json.MarshalIndent(bootstrap, "", "  ")
	if err != nil {
		return runtime.Mount{}, err
	}
	if err := s.root.AtomicWrite(bootstrapDir+"/bootstrap.json", append(raw, '\n'), 0o644); err != nil {
		return runtime.Mount{}, err
	}
	hostPath, _ := s.root.HostPath(bootstrapDir)
	return runtime.Mount{Source: hostPath, Target: "/run/cohotfs/bootstrap", ReadOnly: true, Type: "bind", Propagation: "rprivate"}, nil
}

func (s *DockerService) Start(ctx context.Context, id, key string) (state.Workspace, error) {
	return s.mutateWorkspace(ctx, id, key, "workspace.start", struct{}{}, func(record state.Workspace) (state.Workspace, error) {
		return s.startLocked(ctx, record)
	})
}

func (s *DockerService) startLocked(ctx context.Context, record state.Workspace) (state.Workspace, error) {
	if err := toolchain.ValidateResources(record.Resources); err != nil {
		return record, fmt.Errorf("toolchain resource identity mismatch: %w", err)
	}
	if record.BootstrapAPI != containeragent.BootstrapAPI {
		return record, apperr.New(apperr.ExitStateConflict, "state_conflict", "workspace %s uses bootstrap API %q; remove and recreate it for %s", record.ID, record.BootstrapAPI, containeragent.BootstrapAPI)
	}
	resuming := record.Status == state.StatusStarting || record.Status == state.StatusSetup
	if !resuming && record.Status != state.StatusStopped && record.Status != state.StatusSetupFailed && record.Status != state.StatusError {
		return record, fmt.Errorf("workspace cannot start from %s", record.Status)
	}
	status, err := s.backend.Inspect(ctx, record.RuntimeRef)
	if err != nil {
		return record, err
	}
	if !status.Exists || validateRuntimeIdentity(record, status) != nil {
		return record, fmt.Errorf("runtime object identity mismatch")
	}
	plan, err := s.loadPlan(record.ID)
	if err != nil {
		return record, err
	}
	releasePins, err := pinReservedWorkspaceMasks(plan)
	if err != nil {
		if !status.Running {
			return record, err
		}
		cleanupCtx := context.WithoutCancel(ctx)
		stopErr := s.backend.Stop(cleanupCtx, record.RuntimeRef, 10*time.Second)
		leaseErr := s.releaseActiveIntegrationLeases(cleanupCtx, &record)
		transitionErr := record.Transition(state.StatusError, s.now())
		saveErr := s.store.SaveWorkspace(record)
		return record, errors.Join(err, stopErr, leaseErr, transitionErr, saveErr)
	}
	defer func() { releasePins() }()
	if err := s.acquireIntegrationLeases(ctx, &record, plan); err != nil {
		return record, fmt.Errorf("acquire host integrations: %w", err)
	}
	if !resuming {
		if err := record.Transition(state.StatusStarting, s.now()); err != nil {
			_ = s.releaseActiveIntegrationLeases(ctx, &record)
			return record, err
		}
		if err := s.store.SaveWorkspace(record); err != nil {
			_ = s.releaseActiveIntegrationLeases(ctx, &record)
			return record, err
		}
	}
	failStarted := func(cause error) (state.Workspace, error) {
		cleanupCtx := context.WithoutCancel(ctx)
		stopErr := s.backend.Stop(cleanupCtx, record.RuntimeRef, 10*time.Second)
		leaseErr := s.releaseActiveIntegrationLeases(cleanupCtx, &record)
		_ = record.Transition(state.StatusError, s.now())
		_ = s.store.SaveWorkspace(record)
		return record, errors.Join(cause, stopErr, leaseErr)
	}
	if !status.Running {
		if err := s.backend.Start(ctx, record.RuntimeRef); err != nil {
			maskErr := validateReservedWorkspaceMasks(plan)
			return failStarted(errors.Join(err, maskErr))
		}
	}
	if err := validateReservedWorkspaceMasks(plan); err != nil {
		return failStarted(err)
	}
	releasePins()
	releasePins = func() {}
	ready, err := s.waitForContainerReady(ctx, record.RuntimeRef)
	if err != nil {
		return failStarted(err)
	}
	record.TCPForwarding = ready.TCPForwarding
	if err := s.pinSSHHostKey(ctx, &record, ready.SSHHostFingerprint); err != nil {
		return failStarted(err)
	}
	status, err = s.backend.Inspect(ctx, record.RuntimeRef)
	if err != nil {
		return failStarted(err)
	}
	releaseSSHTransportResources(&record, s.now())
	identity, err := proc.ReadSocket(plan.SSH.SocketPath, record.OwnerUID)
	if err != nil {
		return failStarted(fmt.Errorf("record workspace SSH socket: %w", err))
	}
	record.Resources = append(record.Resources, state.ExternalResource{
		Type: "ssh_socket", ID: identity.Path, AcquiredAt: s.now().UTC(),
		Identity: map[string]string{
			"path": identity.Path, "uid": strconv.Itoa(identity.UID), "dev": strconv.FormatUint(identity.Dev, 10),
			"inode": strconv.FormatUint(identity.Inode, 10), "mode": strconv.FormatUint(uint64(identity.Mode), 10), "nonce": record.RuntimeRef.Nonce,
		},
	})
	shouldRunSetup, err := setupservice.ShouldRun(plan.Setup.Mode, record.Setup, false, false)
	if err != nil {
		return failStarted(err)
	}
	if shouldRunSetup {
		validation, validationErr := setupservice.Validate(record.CanonicalSource, plan.Setup, record.ImageDigest, plan.Image.BootstrapAPI, record.OwnerUID, record.OwnerGID)
		if validationErr != nil {
			if transitionErr := record.Transition(state.StatusSetup, s.now()); transitionErr == nil {
				record.Setup = state.SetupResult{Succeeded: false, ExitCode: -1, Output: validationErr.Error(), FinishedAt: s.now().UTC()}
				_ = record.Transition(state.StatusSetupFailed, s.now())
				_ = s.store.SaveWorkspace(record)
			}
			return s.stopSetupFailed(ctx, record, validationErr)
		}
		setupRunner := setupservice.NewService(s.store, s.backend)
		if setupErr := setupRunner.RunLocked(ctx, &record, plan.Setup, validation, false, false); setupErr != nil {
			return s.stopSetupFailed(ctx, record, setupErr)
		}
	}
	if err := record.Transition(state.StatusReady, s.now()); err != nil {
		return failStarted(err)
	}
	return record, s.store.SaveWorkspace(record)
}

func (s *DockerService) stopSetupFailed(ctx context.Context, record state.Workspace, cause error) (state.Workspace, error) {
	stopErr := s.backend.Stop(context.WithoutCancel(ctx), record.RuntimeRef, 10*time.Second)
	leaseErr := s.releaseActiveIntegrationLeases(context.WithoutCancel(ctx), &record)
	if stopErr != nil {
		_ = record.Transition(state.StatusError, s.now())
	}
	saveErr := s.store.SaveWorkspace(record)
	return record, errors.Join(cause, stopErr, leaseErr, saveErr)
}

func (s *DockerService) Stop(ctx context.Context, id, key string, timeout time.Duration) (state.Workspace, error) {
	body := struct {
		Timeout string `json:"timeout"`
	}{Timeout: timeout.String()}
	return s.mutateWorkspace(ctx, id, key, "workspace.stop", body, func(record state.Workspace) (state.Workspace, error) {
		return s.stopLocked(ctx, record, timeout)
	})
}

func (s *DockerService) Restart(ctx context.Context, id, key string, timeout time.Duration) (state.Workspace, error) {
	body := struct {
		Timeout string `json:"timeout"`
	}{Timeout: timeout.String()}
	return s.mutateWorkspace(ctx, id, key, "workspace.restart", body, func(record state.Workspace) (state.Workspace, error) {
		if record.Status == state.StatusStarting || record.Status == state.StatusSetup {
			return s.startLocked(ctx, record)
		}
		stopped, err := s.stopLocked(ctx, record, timeout)
		if err != nil {
			return stopped, err
		}
		return s.startLocked(ctx, stopped)
	})
}

func (s *DockerService) stopLocked(ctx context.Context, record state.Workspace, timeout time.Duration) (state.Workspace, error) {
	if record.Status == state.StatusStopped {
		if err := s.releaseActiveIntegrationLeases(ctx, &record); err != nil {
			return record, err
		}
		return record, nil
	}
	status, err := s.backend.Inspect(ctx, record.RuntimeRef)
	if err != nil || !status.Exists || validateRuntimeIdentity(record, status) != nil {
		return record, fmt.Errorf("runtime object identity mismatch")
	}
	if record.Status != state.StatusStopping {
		if err := record.Transition(state.StatusStopping, s.now()); err != nil {
			return record, err
		}
		if err := s.store.SaveWorkspace(record); err != nil {
			return record, err
		}
	}
	if status.Running {
		if err := s.backend.Stop(ctx, record.RuntimeRef, timeout); err != nil {
			_ = record.Transition(state.StatusError, s.now())
			_ = s.store.SaveWorkspace(record)
			return record, err
		}
	}
	if err := s.releaseActiveIntegrationLeases(ctx, &record); err != nil {
		_ = record.Transition(state.StatusError, s.now())
		_ = s.store.SaveWorkspace(record)
		return record, err
	}
	releaseSSHTransportResources(&record, s.now())
	if err := record.Transition(state.StatusStopped, s.now()); err != nil {
		return record, err
	}
	return record, s.store.SaveWorkspace(record)
}

func releaseSSHTransportResources(record *state.Workspace, now time.Time) {
	released := now.UTC()
	for index := range record.Resources {
		resource := &record.Resources[index]
		if resource.Type == "ssh_socket" && resource.ReleasedAt == nil {
			resource.ReleasedAt = &released
		}
	}
}

func (s *DockerService) RotateSSHHostKey(ctx context.Context, id, key string) (state.Workspace, error) {
	return s.mutateWorkspace(ctx, id, key, "workspace.rotate-host-key", struct{}{}, func(record state.Workspace) (state.Workspace, error) {
		return s.rotateSSHHostKeyLocked(ctx, record)
	})
}

func (s *DockerService) rotateSSHHostKeyLocked(ctx context.Context, record state.Workspace) (state.Workspace, error) {
	if record.Status != state.StatusReady {
		return record, fmt.Errorf("workspace host key can rotate only from ready")
	}
	result, err := s.backend.ExecSync(ctx, record.RuntimeRef, runtime.ExecRequest{
		Argv: []string{"/bin/rm", "-f", containerSSHHostPrivateKey, containerSSHHostPublicKey},
		User: "0", Timeout: 5 * time.Second, OutputLimit: 16 << 10,
	})
	if err != nil {
		return record, fmt.Errorf("remove workspace SSH host key: %w", err)
	}
	if result.ExitCode != 0 || result.Truncated {
		return record, fmt.Errorf("remove workspace SSH host key failed with exit code %d", result.ExitCode)
	}
	record, err = s.stopLocked(ctx, record, 10*time.Second)
	if err != nil {
		return record, err
	}
	if err := s.removeSSHHostKey(&record); err != nil {
		return record, err
	}
	record.SSHHostFingerprint = ""
	if err := s.store.SaveWorkspace(record); err != nil {
		return record, err
	}
	return s.startLocked(ctx, record)
}

func (s *DockerService) cleanupPersistentSystem(ctx context.Context, record state.Workspace, running bool) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if !running {
		plan, err := s.loadPlan(record.ID)
		if err != nil {
			return err
		}
		releasePins, err := pinReservedWorkspaceMasks(plan)
		if err != nil {
			return err
		}
		defer func() { releasePins() }()
		if err := s.backend.Start(ctx, record.RuntimeRef); err != nil {
			maskErr := validateReservedWorkspaceMasks(plan)
			stopErr := s.backend.Stop(cleanupCtx, record.RuntimeRef, 10*time.Second)
			return errors.Join(fmt.Errorf("start workspace for system cleanup: %w", err), maskErr, stopErr)
		}
		if err := validateReservedWorkspaceMasks(plan); err != nil {
			stopErr := s.backend.Stop(cleanupCtx, record.RuntimeRef, 10*time.Second)
			return errors.Join(err, stopErr)
		}
		releasePins()
		releasePins = func() {}
	}
	cleanupErr := func() error {
		ready, err := s.waitForContainerReady(ctx, record.RuntimeRef)
		if err != nil {
			return err
		}
		if ready.SSHHostFingerprint != record.SSHHostFingerprint {
			return fmt.Errorf("workspace SSH host key does not match recorded identity")
		}
		result, err := s.backend.ExecSync(ctx, record.RuntimeRef, runtime.ExecRequest{
			Argv: []string{containerAgentExecutable, "cleanup-system", "--fingerprint", record.SSHHostFingerprint}, User: "0", Timeout: 10 * time.Second, OutputLimit: 16 << 10,
		})
		if err != nil {
			return err
		}
		if result.Truncated {
			return fmt.Errorf("system cleanup output was truncated")
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("system cleanup exited %d: %s", result.ExitCode, strings.TrimSpace(string(result.Output)))
		}
		return nil
	}()
	stopErr := s.backend.Stop(cleanupCtx, record.RuntimeRef, 10*time.Second)
	return errors.Join(cleanupErr, stopErr)
}

func (s *DockerService) Remove(ctx context.Context, id, key string) error {
	return s.removeOperation(ctx, id, key, "workspace.remove")
}

func (s *DockerService) Recover(ctx context.Context, id, key string) error {
	return s.removeOperation(ctx, id, key, "workspace.recover")
}

func (s *DockerService) removeOperation(ctx context.Context, id, key, operation string) error {
	_, err := s.mutateWorkspace(ctx, id, key, operation, struct{}{}, func(record state.Workspace) (state.Workspace, error) {
		return s.removeLocked(ctx, record)
	})
	if err != nil {
		return err
	}
	return s.store.RemoveWorkspaceStateArtifacts(id)
}

func (s *DockerService) removeLocked(ctx context.Context, record state.Workspace) (state.Workspace, error) {
	id := record.ID
	if err := transitionToRemoving(&record, s.now()); err != nil {
		return record, err
	}
	if err := s.store.SaveWorkspace(record); err != nil {
		return record, err
	}
	if record.RuntimeRef.Backend != "" {
		status, err := s.backend.Inspect(ctx, record.RuntimeRef)
		if err != nil {
			return record, err
		}
		if status.Exists && validateRuntimeIdentity(record, status) != nil {
			return record, fmt.Errorf("runtime object identity mismatch; recovery required")
		}
		if status.Exists {
			if record.SSHHostFingerprint != "" {
				if err := s.cleanupPersistentSystem(ctx, record, status.Running); err != nil {
					return record, err
				}
			} else if status.Running {
				if err := s.backend.Stop(ctx, record.RuntimeRef, 10*time.Second); err != nil {
					return record, err
				}
			}
			if err := s.backend.Delete(ctx, record.RuntimeRef); err != nil {
				return record, err
			}
		}
	}
	if hasActiveIntegrationLease(record) {
		host, hostErr := s.ensureIntegrationHost(ctx)
		if hostErr != nil {
			return record, hostErr
		}
		if err := s.releaseIntegrationLeases(ctx, &record, host); err != nil {
			return record, err
		}
	}
	if err := s.removeSSHHostKey(&record); err != nil {
		return record, err
	}
	if err := toolchain.Deactivate(record.Resources); err != nil {
		for index := range record.Resources {
			if record.Resources[index].Type == "mount" && record.Resources[index].ReleasedAt == nil {
				record.Resources[index].Quarantined = true
			}
		}
		_ = s.store.SaveWorkspace(record)
		return record, err
	}
	released := s.now().UTC()
	for index := range record.Resources {
		if record.Resources[index].Type == "mount" && record.Resources[index].ReleasedAt == nil {
			record.Resources[index].ReleasedAt = &released
		}
	}
	if err := s.store.SaveWorkspace(record); err != nil {
		return record, err
	}
	if err := s.root.RemoveTree(filepath.Join("run", "workspaces", id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	if err := s.root.RemoveTree(filepath.Join("workspaces", id)); err != nil {
		return record, err
	}
	return record, nil
}

func transitionToRemoving(record *state.Workspace, now time.Time) error {
	switch record.Status {
	case state.StatusRemoving:
		return nil
	case state.StatusReady:
		if err := record.Transition(state.StatusStopping, now); err != nil {
			return err
		}
		if err := record.Transition(state.StatusStopped, now); err != nil {
			return err
		}
	case state.StatusStopping:
		if err := record.Transition(state.StatusStopped, now); err != nil {
			return err
		}
	case state.StatusCreating, state.StatusStarting, state.StatusSetup:
		if err := record.Transition(state.StatusError, now); err != nil {
			return err
		}
	}
	return record.Transition(state.StatusRemoving, now)
}
