// Package workspace is the authoritative in-process lifecycle coordinator.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/audit"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/reconcile"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
)

const (
	LabelOwnerUID      = "io.cohotfs.owner-uid"
	LabelWorkspaceID   = "io.cohotfs.workspace-id"
	LabelManifest      = "io.cohotfs.manifest-digest"
	LabelCreationNonce = "io.cohotfs.creation-nonce"
)

type Service struct {
	store    *state.Store
	backends map[string]runtime.Lifecycle
	audit    *audit.Logger
	now      func() time.Time
}

func NewService(store *state.Store, backends map[string]runtime.Lifecycle, logger *audit.Logger) *Service {
	if backends == nil {
		backends = make(map[string]runtime.Lifecycle)
	}
	return &Service{store: store, backends: backends, audit: logger, now: time.Now}
}

type ReconcileReport struct {
	WorkspaceID string   `json:"workspaceID"`
	Matched     []string `json:"matched"`
	Missing     []string `json:"missing"`
	Quarantined []string `json:"quarantined"`
}

func (s *Service) ReconcileAll(ctx context.Context) ([]ReconcileReport, error) {
	workspaces, err := s.store.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	var reports []ReconcileReport
	for _, record := range workspaces {
		if record.Terminal() {
			if record.Quarantined {
				report := ReconcileReport{WorkspaceID: record.ID, Quarantined: []string{"workspace:" + record.ID}}
				reports = append(reports, report)
				return reports, apperr.New(apperr.ExitPartialCleanup, "quarantined", "resource identity mismatch requires recovery")
			}
			continue
		}
		lock, err := s.store.LockWorkspace(record.ID)
		if err != nil {
			return reports, err
		}
		report, reconcileErr := s.reconcileLocked(ctx, &record)
		_ = lock.Close()
		reports = append(reports, report)
		if reconcileErr != nil {
			return reports, reconcileErr
		}
	}
	return reports, nil
}

func (s *Service) Reconcile(ctx context.Context, id string) (ReconcileReport, error) {
	lock, err := s.store.LockWorkspace(id)
	if err != nil {
		return ReconcileReport{}, err
	}
	defer lock.Close()
	record, err := s.store.LoadWorkspace(id)
	if err != nil {
		return ReconcileReport{}, err
	}
	return s.reconcileLocked(ctx, &record)
}

func (s *Service) reconcileLocked(ctx context.Context, record *state.Workspace) (ReconcileReport, error) {
	report := ReconcileReport{WorkspaceID: record.ID}
	changed := false
	if record.RuntimeRef.Backend != "" {
		backend := s.backends[record.Backend]
		if backend == nil {
			return report, apperr.New(apperr.ExitUnavailable, "backend_unavailable", "backend %s is unavailable", record.Backend)
		}
		status, err := backend.Inspect(ctx, record.RuntimeRef)
		if err != nil {
			return report, apperr.Wrap(apperr.ExitRuntime, "runtime", err, "inspect runtime object: %v", err)
		}
		if !status.Exists {
			report.Missing = append(report.Missing, "runtime:"+record.RuntimeRef.Backend)
			if !record.Terminal() && record.Status != state.StatusRemoving {
				_ = record.Transition(state.StatusError, s.now())
				changed = true
			}
		} else if err := validateRuntimeIdentity(*record, status); err != nil {
			report.Quarantined = append(report.Quarantined, "runtime:"+record.RuntimeRef.Backend)
			_ = record.Transition(state.StatusError, s.now())
			changed = true
		} else {
			report.Matched = append(report.Matched, "runtime:"+record.RuntimeRef.Backend)
		}
	}
	for index := range record.Resources {
		resource := &record.Resources[index]
		if resource.ReleasedAt != nil {
			continue
		}
		wasQuarantined := resource.Quarantined
		matched, missing, err := validateExternalResource(*resource)
		if err != nil {
			resource.Quarantined = true
			report.Quarantined = append(report.Quarantined, resource.Type+":"+resource.ID)
			changed = true
			continue
		}
		if wasQuarantined {
			resource.Quarantined = false
			changed = true
		}
		if missing {
			report.Missing = append(report.Missing, resource.Type+":"+resource.ID)
			continue
		}
		if matched {
			report.Matched = append(report.Matched, resource.Type+":"+resource.ID)
		}
	}
	if len(report.Quarantined) != 0 {
		_ = record.Transition(state.StatusError, s.now())
		changed = true
	}
	quarantined := len(report.Quarantined) != 0
	if record.Quarantined != quarantined {
		record.Quarantined = quarantined
		changed = true
	}
	if changed {
		if err := s.store.SaveWorkspace(*record); err != nil {
			return report, err
		}
	}
	if len(report.Quarantined) != 0 {
		return report, apperr.New(apperr.ExitPartialCleanup, "quarantined", "resource identity mismatch requires recovery")
	}
	return report, nil
}

func validateRuntimeIdentity(record state.Workspace, status runtime.WorkspaceStatus) error {
	expected := map[string]string{
		LabelOwnerUID: strconv.Itoa(record.OwnerUID), LabelWorkspaceID: record.ID,
		LabelManifest: record.ManifestDigest, LabelCreationNonce: record.RuntimeRef.Nonce,
	}
	for key, value := range expected {
		if status.Labels[key] != value {
			return fmt.Errorf("runtime label %s mismatch", key)
		}
	}
	return nil
}

func validateExternalResource(resource state.ExternalResource) (matched, missing bool, err error) {
	switch resource.Type {
	case "process":
		identity, parseErr := processIdentity(resource)
		if parseErr != nil {
			return false, false, parseErr
		}
		if err := proc.Matches(identity); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, true, nil
			}
			return false, false, err
		}
		return true, false, nil
	case "socket", "ssh_socket", "host-lease":
		identity, parseErr := socketIdentity(resource)
		if parseErr != nil {
			return false, false, parseErr
		}
		if err := proc.ValidateSocket(identity); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, true, nil
			}
			return false, false, err
		}
		return true, false, nil
	case "mount":
		identity, parseErr := mountIdentity(resource)
		if parseErr != nil {
			return false, false, parseErr
		}
		if err := reconcile.ValidateMount(identity); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, true, nil
			}
			return false, false, err
		}
		return true, false, nil
	case "ssh_known_hosts":
		data, readErr := os.ReadFile(resource.Identity["path"])
		if errors.Is(readErr, os.ErrNotExist) {
			return false, true, nil
		}
		if readErr != nil {
			return false, false, readErr
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != resource.Identity["sha256"] {
			return false, false, fmt.Errorf("known-host identity mismatch")
		}
		return true, false, nil
	default:
		return false, false, fmt.Errorf("unrecognized resource type")
	}
}

func processIdentity(resource state.ExternalResource) (proc.Identity, error) {
	pid, err := strconv.Atoi(resource.Identity["pid"])
	if err != nil {
		return proc.Identity{}, err
	}
	uid, err := strconv.Atoi(resource.Identity["uid"])
	if err != nil {
		return proc.Identity{}, err
	}
	start, err := strconv.ParseUint(resource.Identity["startTicks"], 10, 64)
	if err != nil {
		return proc.Identity{}, err
	}
	return proc.Identity{PID: pid, UID: uid, StartTicks: start, Executable: resource.Identity["executable"], ExecutableDigest: resource.Identity["executableDigest"]}, nil
}

func socketIdentity(resource state.ExternalResource) (proc.SocketIdentity, error) {
	uid, err := strconv.Atoi(resource.Identity["uid"])
	if err != nil {
		return proc.SocketIdentity{}, err
	}
	dev, err := strconv.ParseUint(resource.Identity["dev"], 10, 64)
	if err != nil {
		return proc.SocketIdentity{}, err
	}
	inode, err := strconv.ParseUint(resource.Identity["inode"], 10, 64)
	if err != nil {
		return proc.SocketIdentity{}, err
	}
	mode, err := strconv.ParseUint(resource.Identity["mode"], 10, 32)
	if err != nil {
		return proc.SocketIdentity{}, err
	}
	return proc.SocketIdentity{Path: resource.Identity["path"], UID: uid, Dev: dev, Inode: inode, Mode: uint32(mode)}, nil
}

func mountIdentity(resource state.ExternalResource) (reconcile.MountIdentity, error) {
	id, err := strconv.Atoi(resource.Identity["mountID"])
	if err != nil {
		return reconcile.MountIdentity{}, err
	}
	return reconcile.MountIdentity{MountID: id, MajorMinor: resource.Identity["majorMinor"], Root: resource.Identity["root"], MountPoint: resource.Identity["mountPoint"], Filesystem: resource.Identity["filesystem"], Source: resource.Identity["source"]}, nil
}

type Mutation func(*state.Workspace) (any, error)

// Mutate serializes a workspace operation, reconciles identities before side
// effects, and binds an idempotency key to the operation body.
func (s *Service) Mutate(ctx context.Context, id, key, operation string, body []byte, mutation Mutation) (json.RawMessage, error) {
	lock, err := s.store.LockWorkspace(id)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	record, err := s.store.LoadWorkspace(id)
	if err != nil {
		return nil, err
	}
	if _, err := s.reconcileLocked(ctx, &record); err != nil {
		return nil, err
	}
	journalOperation, replay, err := s.store.BeginOperation(id, key, operation, body, s.now())
	if err != nil {
		return nil, err
	}
	if replay {
		var compact bytes.Buffer
		if len(journalOperation.Result) != 0 {
			if err := json.Compact(&compact, journalOperation.Result); err != nil {
				return nil, err
			}
		}
		result := json.RawMessage(compact.Bytes())
		if journalOperation.Status == state.OperationFailed {
			return result, fmt.Errorf("previous operation failed: %s", journalOperation.Error)
		}
		return result, nil
	}
	result, mutationErr := mutation(&record)
	if mutationErr == nil {
		mutationErr = s.store.SaveWorkspace(record)
	}
	finishErr := s.store.FinishOperation(id, key, result, mutationErr, s.now())
	if s.audit != nil {
		resultText := "success"
		category := ""
		if mutationErr != nil {
			resultText = "failure"
			category = apperr.Category(mutationErr)
		}
		_ = s.audit.Append(audit.Event{Operation: operation, WorkspaceID: id, Result: resultText, ErrorCategory: category})
	}
	if mutationErr != nil {
		return nil, mutationErr
	}
	if finishErr != nil {
		return nil, finishErr
	}
	return json.Marshal(result)
}
