package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
)

type fixtureIntegrationHost struct {
	failAt   int
	acquires []api.LeaseRequest
	active   map[string]api.LeaseSummary
	released []string
}

func (h *fixtureIntegrationHost) Acquire(_ context.Context, request api.LeaseRequest) (api.LeaseResponse, error) {
	h.acquires = append(h.acquires, request)
	if h.failAt != 0 && len(h.acquires) == h.failAt {
		return api.LeaseResponse{}, fmt.Errorf("fixture lease failure")
	}
	id := fmt.Sprintf("lease-%d", len(h.acquires))
	if h.active == nil {
		h.active = map[string]api.LeaseSummary{}
	}
	h.active[id] = api.LeaseSummary{LeaseID: id, WorkspaceID: request.WorkspaceID, Kind: request.Kind}
	return api.LeaseResponse{LeaseID: id, Endpoint: "/run/cohotfs/host/" + string(request.Kind) + ".sock"}, nil
}

func (h *fixtureIntegrationHost) Release(_ context.Context, request api.ReleaseRequest) error {
	if _, ok := h.active[request.LeaseID]; !ok {
		return fmt.Errorf("unknown fixture lease")
	}
	delete(h.active, request.LeaseID)
	h.released = append(h.released, request.LeaseID)
	return nil
}

func (h *fixtureIntegrationHost) Status(context.Context) (api.HostStatus, error) {
	status := api.HostStatus{}
	for _, lease := range h.active {
		status.Leases = append(status.Leases, lease)
	}
	return status, nil
}

func TestIntegrationLeaseFailureRollsBackBeforeContainerStart(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &lifecycleBackend{}
	service := NewDockerService(root, store, backend)
	workspace := config.BuiltinWorkspace("integrated", "example.invalid/base:dev")
	workspace.Spec.Setup.Mode = "manual"
	workspace.Spec.Integrations.Browser = config.BrowserSpec{Enabled: true, Platform: "linux"}
	workspace.Spec.Integrations.GitCredentials = config.GitCredentialsSpec{Enabled: true, AllowedContexts: []string{"https://example.com/org"}}
	workspace.Spec.Integrations.Agents.Codex.Enabled = true
	publicKey := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicKey, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockMockMockMockMockMockMockMockMockMock test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	image := runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootstrapAPI: containeragent.BootstrapAPI}
	record, err := service.Create(context.Background(), CreateRequest{OperationKey: t.Name() + "/create", Workspace: workspace, CanonicalSource: t.TempDir(), ManifestDigest: "manifest", OwnerUID: 1000, OwnerGID: 1000, Image: image, BackendInfo: availableDocker(), SSHSocketPath: testSSHSocket(t), BootstrapSource: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	foundHostMount := false
	for _, mount := range backend.created.Mounts {
		foundHostMount = foundHostMount || mount.Target == "/run/cohotfs/host" && mount.ReadOnly
	}
	if !foundHostMount {
		t.Fatal("explicit integrations did not add a read-only host socket mount")
	}
	host := &fixtureIntegrationHost{failAt: 2, active: map[string]api.LeaseSummary{}}
	service.SetIntegrationHost(host)
	record, err = service.Start(context.Background(), record.ID, t.Name()+"/start-failed")
	if err == nil || backend.started || record.Status != state.StatusStopped || len(host.released) != 1 || len(host.active) != 0 {
		t.Fatalf("failed start record=%s backend=%v released=%v active=%v err=%v", record.Status, backend.started, host.released, host.active, err)
	}

	host.failAt = 0
	host.acquires = nil
	record, err = service.Start(context.Background(), record.ID, t.Name()+"/start-success")
	if err != nil || record.Status != state.StatusReady || len(host.active) != 3 {
		t.Fatalf("start record=%s active=%v err=%v", record.Status, host.active, err)
	}
	record, err = service.Stop(context.Background(), record.ID, t.Name()+"/stop", time.Second)
	if err != nil || record.Status != state.StatusStopped || len(host.active) != 0 {
		t.Fatalf("stop record=%s active=%v err=%v", record.Status, host.active, err)
	}
}
