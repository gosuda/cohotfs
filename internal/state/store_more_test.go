package state

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/hostroot"
)

func testStore(t *testing.T) (*Store, func()) {
	t.Helper()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	return store, func() { _ = root.Close() }
}

func TestOperationIdempotencyBinding(t *testing.T) {
	store, closeStore := testStore(t)
	defer closeStore()
	id, err := NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	operation, replay, err := store.BeginOperation(id, "request-1", "create", []byte(`{"name":"one"}`), now)
	if err != nil || replay || operation.Status != OperationRunning {
		t.Fatalf("begin = %#v, replay=%v, err=%v", operation, replay, err)
	}
	if err := store.FinishOperation(id, "request-1", map[string]string{"id": id}, nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	operation, replay, err = store.BeginOperation(id, "request-1", "create", []byte(`{"name":"one"}`), now.Add(2*time.Second))
	if err != nil || !replay || operation.Status != OperationSucceeded {
		t.Fatalf("replay = %#v, replay=%v, err=%v", operation, replay, err)
	}
	_, _, err = store.BeginOperation(id, "request-1", "create", []byte(`{"name":"two"}`), now)
	if err == nil || apperr.Code(err) != apperr.ExitStateConflict {
		t.Fatalf("reused key error = %v", err)
	}
}

func TestWorkspaceLockAndLifecycle(t *testing.T) {
	store, closeStore := testStore(t)
	defer closeStore()
	id, _ := NewWorkspaceID()
	first, err := store.LockWorkspace(id)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := store.LockWorkspace(id); err == nil || apperr.Code(err) != apperr.ExitStateConflict {
		t.Fatalf("second lock error = %v", err)
	}

	workspace := Workspace{ID: id, Name: "test", BootstrapAPI: "bootstrap", TCPForwarding: true, Status: StatusCreating, CreatedAt: time.Unix(1, 0)}
	if err := workspace.Transition(StatusReady, time.Unix(2, 0)); err == nil {
		t.Fatal("accepted creating -> ready")
	}
	if err := workspace.Transition(StatusStarting, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadWorkspace(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStarting || got.SchemaVersion != SchemaVersion || got.BootstrapAPI != "bootstrap" || !got.TCPForwarding {
		t.Fatalf("loaded workspace = %#v", got)
	}
}

func TestLoadWorkspaceRejectsIdentityMismatch(t *testing.T) {
	store, closeStore := testStore(t)
	defer closeStore()
	id, _ := NewWorkspaceID()
	other, _ := NewWorkspaceID()
	if err := store.SaveWorkspace(Workspace{ID: id, Status: StatusCreating}); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.LoadWorkspace(id)
	if err != nil {
		t.Fatal(err)
	}
	workspace.ID = other
	data, err := json.Marshal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.root.AtomicWrite("state/workspaces/"+id+"/workspace.json", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadWorkspace(id); err == nil {
		t.Fatal("accepted mismatched state")
	}
}
