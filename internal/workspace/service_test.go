package workspace

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
)

type fakeBackend struct{ status runtime.WorkspaceStatus }

func (f *fakeBackend) Probe(context.Context) (runtime.BackendInfo, error) {
	return runtime.BackendInfo{}, nil
}
func (f *fakeBackend) Pull(context.Context, runtime.PullRequest) (runtime.ResolvedImage, error) {
	return runtime.ResolvedImage{}, nil
}
func (f *fakeBackend) Create(context.Context, runtime.WorkspaceSpec) (runtime.WorkspaceRef, error) {
	return runtime.WorkspaceRef{}, nil
}
func (f *fakeBackend) Start(context.Context, runtime.WorkspaceRef) error { return nil }
func (f *fakeBackend) ExecSync(context.Context, runtime.WorkspaceRef, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, nil
}
func (f *fakeBackend) Inspect(context.Context, runtime.WorkspaceRef) (runtime.WorkspaceStatus, error) {
	return f.status, nil
}
func (f *fakeBackend) Stop(context.Context, runtime.WorkspaceRef, time.Duration) error { return nil }
func (f *fakeBackend) Delete(context.Context, runtime.WorkspaceRef) error              { return nil }

func newTestService(t *testing.T, backend runtime.Lifecycle) (*Service, *state.Store, func()) {
	t.Helper()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backends := map[string]runtime.Lifecycle{}
	if backend != nil {
		backends["docker"] = backend
	}
	service := NewService(store, backends, nil)
	service.now = func() time.Time { return time.Unix(20, 0) }
	return service, store, func() { _ = root.Close() }
}

func TestReconcileMatchesRuntimeAndProcessIdentity(t *testing.T) {
	id, _ := state.NewWorkspaceID()
	nonce := "nonce"
	labels := map[string]string{LabelOwnerUID: strconv.Itoa(1000), LabelWorkspaceID: id, LabelManifest: "manifest", LabelCreationNonce: nonce}
	backend := &fakeBackend{status: runtime.WorkspaceStatus{Exists: true, Running: true, Labels: labels}}
	service, store, closeService := newTestService(t, backend)
	defer closeService()
	identity, err := proc.CurrentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	record := state.Workspace{ID: id, OwnerUID: 1000, Backend: "docker", ManifestDigest: "manifest", RuntimeRef: runtime.WorkspaceRef{Backend: "docker", Nonce: nonce}, Status: state.StatusStarting,
		Resources: []state.ExternalResource{{Type: "process", ID: "helper", Identity: map[string]string{
			"pid": strconv.Itoa(identity.PID), "uid": strconv.Itoa(identity.UID), "startTicks": strconv.FormatUint(identity.StartTicks, 10), "executable": identity.Executable, "executableDigest": identity.ExecutableDigest,
		}}},
	}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	report, err := service.Reconcile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matched) != 2 || len(report.Quarantined) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestReconcileQuarantinesRuntimeIdentityMismatch(t *testing.T) {
	id, _ := state.NewWorkspaceID()
	backend := &fakeBackend{status: runtime.WorkspaceStatus{Exists: true, Labels: map[string]string{LabelWorkspaceID: "other"}}}
	service, store, closeService := newTestService(t, backend)
	defer closeService()
	record := state.Workspace{ID: id, OwnerUID: 1000, Backend: "docker", ManifestDigest: "manifest", RuntimeRef: runtime.WorkspaceRef{Backend: "docker", Nonce: "nonce"}, Status: state.StatusStarting}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	report, err := service.Reconcile(context.Background(), id)
	if err == nil || apperr.Code(err) != apperr.ExitPartialCleanup || len(report.Quarantined) != 1 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	updated, err := store.LoadWorkspace(id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != state.StatusError || !updated.Quarantined {
		t.Fatalf("quarantined state = %#v", updated)
	}
	if _, err := service.ReconcileAll(context.Background()); apperr.Code(err) != apperr.ExitPartialCleanup {
		t.Fatalf("durable quarantine was not enforced: %v", err)
	}
	backend.status.Labels = map[string]string{
		LabelOwnerUID: "1000", LabelWorkspaceID: id, LabelManifest: "manifest", LabelCreationNonce: "nonce",
	}
	if _, err := service.Reconcile(context.Background(), id); err != nil {
		t.Fatalf("reconcile corrected identity: %v", err)
	}
	updated, err = store.LoadWorkspace(id)
	if err != nil || updated.Quarantined {
		t.Fatalf("quarantine was not cleared: %#v, %v", updated, err)
	}
	if _, err := service.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("cleared quarantine remained blocked: %v", err)
	}
}

func TestMutateReplaysCompletedOperation(t *testing.T) {
	service, store, closeService := newTestService(t, nil)
	defer closeService()
	id, _ := state.NewWorkspaceID()
	if err := store.SaveWorkspace(state.Workspace{ID: id, Status: state.StatusStopped}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	mutation := func(record *state.Workspace) (any, error) {
		calls++
		return map[string]string{"id": record.ID}, nil
	}
	first, err := service.Mutate(context.Background(), id, "key", "recover", []byte("body"), mutation)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Mutate(context.Background(), id, "key", "recover", []byte("body"), mutation)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || string(first) != string(second) {
		t.Fatalf("calls=%d first=%s second=%s", calls, first, second)
	}
}
