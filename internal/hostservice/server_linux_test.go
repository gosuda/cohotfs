//go:build linux

package hostservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/proc"
)

func TestServerAuthorizesSameUIDAndDrainsLeases(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	server.Register(api.LeaseChrome, func(_ context.Context, request api.LeaseRequest) (Lease, api.LeaseResponse, error) {
		return Lease{Summary: api.LeaseSummary{ExpiresAt: time.Now().Add(time.Minute)}, Close: func(context.Context) error {
			closed.Store(true)
			return nil
		}}, api.LeaseResponse{Endpoint: "test"}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(ctx) }()

	client, err := NewClient(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var status api.HostStatus
	for {
		status, err = client.Status(context.Background())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server readiness: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.UID != os.Getuid() || status.Root != root.Path() || status.Protocol != api.ProtocolVersion {
		t.Fatalf("status identity = %#v", status)
	}
	lease, err := client.Acquire(context.Background(), api.LeaseRequest{IdempotencyKey: "request-1", WorkspaceID: "workspace", Kind: api.LeaseChrome, Parameters: map[string]string{"opaque": "fixture-token"}})
	if err != nil || lease.LeaseID == "" {
		t.Fatalf("acquire = %#v, %v", lease, err)
	}
	replay, err := client.Acquire(context.Background(), api.LeaseRequest{IdempotencyKey: "request-1", WorkspaceID: "workspace", Kind: api.LeaseChrome, Parameters: map[string]string{"opaque": "fixture-token"}})
	if err != nil || replay.LeaseID != lease.LeaseID {
		t.Fatalf("idempotent acquire = %#v, %v", replay, err)
	}
	if _, err := client.Acquire(context.Background(), api.LeaseRequest{IdempotencyKey: "request-1", WorkspaceID: "workspace", Kind: api.LeaseChrome, Parameters: map[string]string{"changed": "true"}}); err == nil {
		t.Fatal("accepted reused lease key with different input")
	}
	persisted, err := root.ReadFile(leaseRecordPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte("fixture-token")) || bytes.Contains(persisted, []byte(`"response"`)) || bytes.Contains(persisted, []byte(`"endpoint"`)) {
		t.Fatalf("host lease record persisted request/response data: %s", persisted)
	}
	if err := client.Stop(context.Background(), false); err == nil {
		t.Fatal("stopped with an active lease without force")
	}
	if err := client.Stop(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
	if !closed.Load() {
		t.Fatal("forced stop did not close lease")
	}
	socketPath, _ := root.SocketPath(socketRelativePath)
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after stop: %v", err)
	}
}

func TestServerRejectsWrongPeerUID(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	handler := server.authorize(func(http.ResponseWriter, *http.Request) { called = true })
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	request = request.WithContext(context.WithValue(request.Context(), peerUIDKey{}, root.UID()+1))
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusForbidden || called {
		t.Fatalf("wrong-UID response=%d called=%v", response.Code, called)
	}
}

func TestServerContextCancellationReturns(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx) }()
	waitForServer(t, root)
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation deadlocked")
	}
}

func TestServerListenerClosureReturns(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx) }()
	waitForServer(t, root)
	if err := server.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("listener closure deadlocked")
	}
}

func waitForServer(t *testing.T, root *hostroot.Root) {
	t.Helper()
	client, err := NewClient(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := client.Status(context.Background()); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("server readiness timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReconcileStaleLeaseRemovesOnlyIdentityMatchedSocket(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	socketPath := filepath.Join(root.Path(), "run", "stale.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	socket, err := proc.ReadSocket(socketPath, root.UID())
	if err != nil {
		t.Fatal(err)
	}
	record := durableLeaseFile{SchemaVersion: 1, Host: proc.Identity{PID: 99999999}, Leases: []durableLease{{Summary: api.LeaseSummary{LeaseID: "stale", WorkspaceID: "workspace", Kind: api.LeaseChrome}, Sockets: []proc.SocketIdentity{socket}}}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.AtomicWrite(leaseRecordPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.reconcileStaleLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket remains: %v", err)
	}
	if _, err := root.ReadFile(leaseRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale lease record remains: %v", err)
	}
}
