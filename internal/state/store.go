// Package state persists versioned Cohotfs records and operation journals.
package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
	"golang.org/x/sys/unix"
)

const (
	SchemaVersion              = 1
	WorkspacePlanSchemaVersion = 2
)

type Status string

const (
	StatusCreating    Status = "creating"
	StatusStarting    Status = "starting"
	StatusSetup       Status = "setup"
	StatusReady       Status = "ready"
	StatusSetupFailed Status = "setup_failed"
	StatusStopping    Status = "stopping"
	StatusStopped     Status = "stopped"
	StatusRemoving    Status = "removing"
	StatusError       Status = "error"
)

var transitions = map[Status]map[Status]bool{
	StatusCreating:    {StatusStarting: true, StatusStopped: true, StatusError: true, StatusRemoving: true},
	StatusStarting:    {StatusSetup: true, StatusReady: true, StatusStopped: true, StatusError: true},
	StatusSetup:       {StatusReady: true, StatusSetupFailed: true, StatusStopped: true, StatusError: true},
	StatusReady:       {StatusSetup: true, StatusStopping: true, StatusError: true},
	StatusSetupFailed: {StatusStarting: true, StatusStopping: true, StatusRemoving: true, StatusError: true},
	StatusStopping:    {StatusStopped: true, StatusError: true},
	StatusStopped:     {StatusStarting: true, StatusRemoving: true, StatusError: true},
	StatusRemoving:    {StatusError: true},
	StatusError:       {StatusStarting: true, StatusStopping: true, StatusRemoving: true},
}

type MountRecord struct {
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	ReadOnly    bool   `json:"readOnly"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type SetupResult struct {
	Digest     string    `json:"digest,omitempty"`
	Succeeded  bool      `json:"succeeded"`
	ExitCode   int       `json:"exitCode,omitempty"`
	Output     string    `json:"output,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

type ExternalResource struct {
	Type        string            `json:"type"`
	ID          string            `json:"id"`
	Identity    map[string]string `json:"identity,omitempty"`
	AcquiredAt  time.Time         `json:"acquiredAt"`
	ReleasedAt  *time.Time        `json:"releasedAt,omitempty"`
	Quarantined bool              `json:"quarantined,omitempty"`
}

type Workspace struct {
	SchemaVersion      int                         `json:"schemaVersion"`
	ID                 string                      `json:"id"`
	Name               string                      `json:"name"`
	OwnerUID           int                         `json:"ownerUID"`
	OwnerGID           int                         `json:"ownerGID"`
	CanonicalSource    string                      `json:"canonicalSource"`
	ManifestDigest     string                      `json:"manifestDigest"`
	Backend            string                      `json:"backend"`
	RuntimeRef         runtime.WorkspaceRef        `json:"runtimeRef"`
	Capabilities       map[runtime.Capability]bool `json:"capabilities,omitempty"`
	ImageDigest        string                      `json:"imageDigest"`
	BootstrapAPI       string                      `json:"bootstrapAPI,omitempty"`
	TCPForwarding      bool                        `json:"tcpForwarding,omitempty"`
	ContainerUID       int                         `json:"containerUID"`
	ContainerGID       int                         `json:"containerGID"`
	Mounts             []MountRecord               `json:"mounts,omitempty"`
	SSHHostFingerprint string                      `json:"sshHostFingerprint,omitempty"`
	Setup              SetupResult                 `json:"setup"`
	IntegrationGrants  map[string]bool             `json:"integrationGrants,omitempty"`
	Resources          []ExternalResource          `json:"resources,omitempty"`
	Quarantined        bool                        `json:"quarantined,omitempty"`
	Status             Status                      `json:"status"`
	CreatedAt          time.Time                   `json:"createdAt"`
	UpdatedAt          time.Time                   `json:"updatedAt"`
}

func (w *Workspace) Transition(next Status, now time.Time) error {
	if w.Status == next {
		return nil
	}
	if !transitions[w.Status][next] {
		return apperr.New(apperr.ExitStateConflict, "state_conflict", "illegal workspace transition %s -> %s", w.Status, next)
	}
	w.Status = next
	w.UpdatedAt = now.UTC()
	return nil
}

func (w Workspace) Terminal() bool {
	return w.Status == StatusReady || w.Status == StatusSetupFailed || w.Status == StatusStopped || w.Status == StatusError
}

type OperationStatus string

const (
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
)

type Operation struct {
	Key        string          `json:"key"`
	Name       string          `json:"name"`
	BodyDigest string          `json:"bodyDigest"`
	Status     OperationStatus `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"startedAt"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
}

type Journal struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Operations    map[string]Operation `json:"operations"`
}

type Store struct{ root *hostroot.Root }

func NewStore(root *hostroot.Root) (*Store, error) {
	for _, path := range []string{"run/locks", "run/locks/workspaces", "run/locks/images", "run/locks/operations"} {
		if err := root.EnsureDir(path, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

var idRE = regexp.MustCompile(`^[a-z2-7]{26}$`)

func NewWorkspaceID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])), nil
}

func WorkspaceIDForOperationKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("operation key is required")
	}
	sum := sha256.Sum256([]byte(key))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])), nil
}

func validateID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid workspace id")
	}
	return nil
}

type Lock struct{ file *os.File }

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

func (s *Store) LockWorkspace(id string) (*Lock, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	return s.lock("run/locks/workspaces/"+id+".lock", "workspace is being modified")
}

func (s *Store) LockOperation(subject, operation string) (*Lock, error) {
	if subject == "" || operation == "" {
		return nil, fmt.Errorf("operation subject and name are required")
	}
	sum := sha256.Sum256([]byte(subject + "\x00" + operation))
	return s.lock("run/locks/operations/"+hex.EncodeToString(sum[:16])+".lock", "operation is already in progress")
}

func (s *Store) LockImage(key string) (*Lock, error) {
	sum := sha256.Sum256([]byte(key))
	return s.lock("run/locks/images/"+hex.EncodeToString(sum[:16])+".lock", "image is being modified")
}

func (s *Store) lock(path, message string) (*Lock, error) {
	file, err := s.root.OpenFile(path, unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, apperr.New(apperr.ExitStateConflict, "state_conflict", "%s", message)
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

func (s *Store) workspaceDir(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	path := "state/workspaces/" + id
	if err := s.root.EnsureDir(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) SaveWorkspace(workspace Workspace) error {
	dir, err := s.workspaceDir(workspace.ID)
	if err != nil {
		return err
	}
	workspace.SchemaVersion = SchemaVersion
	data, err := json.MarshalIndent(workspace, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.root.AtomicWrite(dir+"/workspace.json", data, 0o600)
}

func (s *Store) SaveWorkspaceArtifact(id, name string, data []byte) error {
	dir, err := s.workspaceDir(id)
	if err != nil {
		return err
	}
	if name != "plan.json" && name != "bootstrap.json" {
		return fmt.Errorf("unsupported workspace artifact")
	}
	return s.root.AtomicWrite(dir+"/"+name, data, 0o600)
}

func (s *Store) LoadWorkspaceArtifact(id, name string) ([]byte, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	if name != "plan.json" && name != "bootstrap.json" {
		return nil, fmt.Errorf("unsupported workspace artifact")
	}
	return s.root.ReadFile("state/workspaces/" + id + "/" + name)
}

func (s *Store) LoadWorkspace(id string) (Workspace, error) {
	if err := validateID(id); err != nil {
		return Workspace{}, err
	}
	data, err := s.root.ReadFile("state/workspaces/" + id + "/workspace.json")
	if err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	if err := json.Unmarshal(data, &workspace); err != nil {
		return Workspace{}, err
	}
	if workspace.SchemaVersion != SchemaVersion || workspace.ID != id {
		return Workspace{}, fmt.Errorf("workspace state identity mismatch")
	}
	return workspace, nil
}

func (s *Store) ListWorkspaces() ([]Workspace, error) {
	path, _ := s.root.HostPath("state/workspaces")
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	workspaces := make([]Workspace, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !idRE.MatchString(entry.Name()) {
			continue
		}
		workspace, err := s.LoadWorkspace(entry.Name())
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].CreatedAt.Before(workspaces[j].CreatedAt) })
	return workspaces, nil
}

func (s *Store) loadJournal(id string) (Journal, error) {
	data, err := s.root.ReadFile("state/workspaces/" + id + "/journal.json")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return Journal{SchemaVersion: SchemaVersion, Operations: map[string]Operation{}}, nil
	}
	if err != nil {
		return Journal{}, err
	}
	var journal Journal
	if err := json.Unmarshal(data, &journal); err != nil {
		return Journal{}, err
	}
	if journal.SchemaVersion != SchemaVersion {
		return Journal{}, fmt.Errorf("unsupported journal schema")
	}
	return journal, nil
}

func (s *Store) saveJournal(id string, journal Journal) error {
	dir, err := s.workspaceDir(id)
	if err != nil {
		return err
	}
	journal.SchemaVersion = SchemaVersion
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return s.root.AtomicWrite(dir+"/journal.json", append(data, '\n'), 0o600)
}

// BeginOperation binds an idempotency key to operation and body digest. A
// completed identical operation is returned for replay without side effects.
func (s *Store) BeginOperation(id, key, name string, body []byte, now time.Time) (Operation, bool, error) {
	if key == "" || name == "" {
		return Operation{}, false, fmt.Errorf("idempotency key and operation are required")
	}
	if _, err := s.workspaceDir(id); err != nil {
		return Operation{}, false, err
	}
	journal, err := s.loadJournal(id)
	if err != nil {
		return Operation{}, false, err
	}
	digest := sha256.Sum256(body)
	digestText := hex.EncodeToString(digest[:])
	if existing, ok := journal.Operations[key]; ok {
		if existing.Name != name || existing.BodyDigest != digestText {
			return Operation{}, false, apperr.New(apperr.ExitStateConflict, "idempotency_conflict", "idempotency key was used with different input")
		}
		if existing.Status == OperationRunning {
			return existing, false, nil
		}
		return existing, true, nil
	}
	operation := Operation{Key: key, Name: name, BodyDigest: digestText, Status: OperationRunning, StartedAt: now.UTC()}
	journal.Operations[key] = operation
	if err := s.saveJournal(id, journal); err != nil {
		return Operation{}, false, err
	}
	return operation, false, nil
}

func (s *Store) FinishOperation(id, key string, result any, operationErr error, now time.Time) error {
	journal, err := s.loadJournal(id)
	if err != nil {
		return err
	}
	operation, ok := journal.Operations[key]
	if !ok || operation.Status != OperationRunning {
		return apperr.New(apperr.ExitStateConflict, "state_conflict", "operation is not running")
	}
	finished := now.UTC()
	operation.FinishedAt = &finished
	if operationErr != nil {
		operation.Status = OperationFailed
		operation.Error = operationErr.Error()
	} else {
		operation.Status = OperationSucceeded
		if result != nil {
			operation.Result, err = json.Marshal(result)
			if err != nil {
				return err
			}
		}
	}
	journal.Operations[key] = operation
	return s.saveJournal(id, journal)
}

func (s *Store) RemoveWorkspaceStateArtifacts(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	var failures []error
	for _, name := range []string{"workspace.json", "plan.json", "bootstrap.json"} {
		err := s.root.Remove("state/workspaces/" + id + "/" + name)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
