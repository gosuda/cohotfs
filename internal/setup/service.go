package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/gosuda/cohotfs/internal/toolchain"
)

type Service struct {
	store   *state.Store
	backend runtime.Lifecycle
	now     func() time.Time
}

func NewService(store *state.Store, backend runtime.Lifecycle) *Service {
	return &Service{store: store, backend: backend, now: time.Now}
}

func ShouldRun(mode string, previous state.SetupResult, explicit, force bool) (bool, error) {
	switch mode {
	case "once":
		if explicit {
			return !previous.Succeeded || force, nil
		}
		return !previous.Succeeded, nil
	case "always":
		return true, nil
	case "manual":
		return explicit, nil
	default:
		return false, fmt.Errorf("unknown setup mode")
	}
}

func (s *Service) Run(ctx context.Context, workspaceID string, setupSpec config.SetupSpec, validation Validation, explicit, force bool) (state.Workspace, error) {
	lock, err := s.store.LockWorkspace(workspaceID)
	if err != nil {
		return state.Workspace{}, err
	}
	defer lock.Close()
	record, err := s.store.LoadWorkspace(workspaceID)
	if err != nil {
		return state.Workspace{}, err
	}
	runErr := s.RunLocked(ctx, &record, setupSpec, validation, explicit, force)
	return record, runErr
}

// RunLocked executes setup while the caller holds the workspace mutation lock.
func (s *Service) RunLocked(ctx context.Context, record *state.Workspace, setupSpec config.SetupSpec, validation Validation, explicit, force bool) error {
	run, err := ShouldRun(setupSpec.Mode, record.Setup, explicit, force)
	if err != nil || !run {
		return err
	}
	if record.Status != state.StatusReady && record.Status != state.StatusStarting && record.Status != state.StatusSetup && record.Status != state.StatusSetupFailed && record.Status != state.StatusStopped {
		return fmt.Errorf("setup cannot run from %s", record.Status)
	}
	environment := []string{"HOME=/home/agent", "XDG_CONFIG_HOME=/home/agent/.config", "XDG_CACHE_HOME=/home/agent/.cache", "XDG_DATA_HOME=/home/agent/.local/share", "TMPDIR=/home/agent/.tmp"}
	if record.IntegrationGrants["hostToolchains"] {
		managed, err := s.setupToolchainEnvironment(record.ID)
		if err != nil {
			return err
		}
		environment = append(environment, managed...)
	}
	if record.Status == state.StatusStopped || record.Status == state.StatusSetupFailed {
		if err := record.Transition(state.StatusStarting, s.now()); err != nil {
			return err
		}
	}
	if record.Status != state.StatusSetup {
		if err := record.Transition(state.StatusSetup, s.now()); err != nil {
			return err
		}
	}
	if err := s.store.SaveWorkspace(*record); err != nil {
		return err
	}
	request := runtime.ExecRequest{
		Argv:       append([]string{"/usr/local/libexec/cohotfs-agent", "setup", "--timeout", setupSpec.Timeout.String(), "--"}, setupSpec.Command...),
		WorkingDir: "/workspace", User: validation.User, Timeout: setupSpec.Timeout + 15*time.Second,
		OutputLimit: 1 << 20,
		Environment: environment,
	}
	result, runErr := s.backend.ExecSync(ctx, record.RuntimeRef, request)
	record.Setup = state.SetupResult{Digest: validation.Digest, Succeeded: runErr == nil && result.ExitCode == 0, ExitCode: result.ExitCode, Output: string(result.Output), Truncated: result.Truncated, FinishedAt: s.now().UTC()}
	if record.Setup.Succeeded {
		if err := record.Transition(state.StatusReady, s.now()); err != nil {
			return err
		}
	} else {
		if err := record.Transition(state.StatusSetupFailed, s.now()); err != nil {
			return err
		}
	}
	if err := s.store.SaveWorkspace(*record); err != nil {
		return err
	}
	if !record.Setup.Succeeded {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("setup exited %d", result.ExitCode)
	}
	return nil
}

func (s *Service) setupToolchainEnvironment(workspaceID string) ([]string, error) {
	raw, err := s.store.LoadWorkspaceArtifact(workspaceID, "plan.json")
	if err != nil {
		return nil, fmt.Errorf("load workspace plan for setup: %w", err)
	}
	var plan struct {
		SchemaVersion int            `json:"schemaVersion"`
		WorkspaceID   string         `json:"workspaceID"`
		Toolchains    toolchain.Plan `json:"toolchains"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("decode workspace plan for setup: %w", err)
	}
	if plan.SchemaVersion != 1 || plan.WorkspaceID != workspaceID {
		return nil, fmt.Errorf("workspace plan identity is invalid")
	}
	environment, err := toolchain.SetupEnvironment(plan.Toolchains.Environment)
	if err != nil {
		return nil, fmt.Errorf("validate setup toolchain environment: %w", err)
	}
	return environment, nil
}
