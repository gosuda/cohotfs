package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type capturedCreate struct {
	container.Config
	HostConfig *container.HostConfig `json:"HostConfig"`
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testAdapter(t *testing.T, capture *capturedCreate, infoRuntimes string) *Adapter {
	t.Helper()
	apiClient, err := client.New(
		client.WithHost("http://docker.test"),
		client.WithAPIVersion("1.55"),
		client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch {
			case request.URL.Path == "/v1.55/containers/create":
				if err := json.NewDecoder(request.Body).Decode(capture); err != nil {
					return nil, err
				}
				return jsonResponse(http.StatusCreated, `{"Id":"container-id","Warnings":[]}`), nil
			case request.URL.Path == "/v1.55/info":
				return jsonResponse(http.StatusOK, fmt.Sprintf(`{"ServerVersion":"29.0","Runtimes":%s}`, infoRuntimes)), nil
			default:
				return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			}
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return newWithClient(apiClient, "unix:///var/run/docker.sock", "runsc")
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func validSpec() runtime.WorkspaceSpec {
	return runtime.WorkspaceSpec{
		WorkspaceID: "workspace", OwnerUID: 1000, OwnerGID: 1000, ManifestDigest: "manifest", CreationNonce: "nonce",
		Image: runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Labels: map[string]string{
			"io.cohotfs.owner-uid": "1000", "io.cohotfs.workspace-id": "workspace",
			"io.cohotfs.manifest-digest": "manifest", "io.cohotfs.creation-nonce": "nonce",
		},
	}
}

func TestCreateDefaultSecurityAndNoResourceLimits(t *testing.T) {
	var capture capturedCreate
	adapter := testAdapter(t, &capture, `{}`)
	ref, err := adapter.Create(context.Background(), validSpec())
	if err != nil {
		t.Fatal(err)
	}
	if ref.IDs["container"] != "container-id" || ref.Nonce != "nonce" {
		t.Fatalf("ref = %#v", ref)
	}
	if capture.HostConfig == nil || capture.HostConfig.Privileged || capture.Config.User != "0:0" || capture.HostConfig.NetworkMode != "bridge" {
		t.Fatalf("unsafe default config: %#v %#v", capture.Config, capture.HostConfig)
	}
	if len(capture.HostConfig.CapAdd) != 0 || len(capture.HostConfig.CapDrop) != 0 {
		t.Fatalf("Cohotfs changed capabilities: add %v drop %v", capture.HostConfig.CapAdd, capture.HostConfig.CapDrop)
	}
	if len(capture.HostConfig.SecurityOpt) != 1 || capture.HostConfig.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("security opts = %v", capture.HostConfig.SecurityOpt)
	}
	if len(capture.HostConfig.Annotations) != 0 {
		t.Fatalf("standard runtime annotations = %#v", capture.HostConfig.Annotations)
	}
	resources := capture.HostConfig.Resources
	if resources.NanoCPUs != 0 || resources.Memory != 0 || resources.MemorySwap != 0 || resources.PidsLimit != nil || len(resources.Ulimits) != 0 {
		t.Fatalf("default resources are constrained: %#v", resources)
	}
}

func TestCreateResumesOnlyIdentityMatchedConflict(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		workspace string
		wantError bool
	}{
		{name: "matched", workspace: "workspace"},
		{name: "mismatched", workspace: "other", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			spec := validSpec()
			labels := map[string]string{
				"io.cohotfs.owner-uid": "1000", "io.cohotfs.workspace-id": testCase.workspace,
				"io.cohotfs.manifest-digest": "manifest", "io.cohotfs.creation-nonce": "nonce",
			}
			labelsRaw, err := json.Marshal(labels)
			if err != nil {
				t.Fatal(err)
			}
			apiClient, err := client.New(
				client.WithHost("http://docker.test"),
				client.WithAPIVersion("1.55"),
				client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					switch {
					case request.Method == http.MethodPost && request.URL.Path == "/v1.55/containers/create":
						_, _ = io.Copy(io.Discard, request.Body)
						return jsonResponse(http.StatusConflict, `{"message":"container name is already in use"}`), nil
					case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/containers/cohotfs-workspace/json"):
						return jsonResponse(http.StatusOK, fmt.Sprintf(`{"Id":"existing-id","Config":{"Labels":%s},"State":{"Running":false}}`, labelsRaw)), nil
					default:
						return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
					}
				})}),
			)
			if err != nil {
				t.Fatal(err)
			}
			var recorded runtime.WorkspaceRef
			spec.Record = func(ref runtime.WorkspaceRef) error {
				recorded = ref
				return nil
			}
			ref, err := newWithClient(apiClient, "unix:///var/run/docker.sock", "").Create(context.Background(), spec)
			if testCase.wantError {
				if err == nil {
					t.Fatalf("accepted conflicting identity: %#v", ref)
				}
				return
			}
			if err != nil || ref.IDs["container"] != "existing-id" || recorded.IDs["container"] != "existing-id" {
				t.Fatalf("recovered ref=%#v recorded=%#v err=%v", ref, recorded, err)
			}
		})
	}
}

func TestCreateMasksNestedHostStateWithReadOnlyTmpfs(t *testing.T) {
	var capture capturedCreate
	adapter := testAdapter(t, &capture, `{}`)
	spec := validSpec()
	spec.Mounts = []runtime.Mount{
		{Source: "/home/user", Target: "/workspace", Type: "bind", Propagation: "rprivate"},
		{Target: "/workspace/.cohotfs", Type: "tmpfs", ReadOnly: true},
	}
	if _, err := adapter.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if len(capture.HostConfig.Mounts) != 2 {
		t.Fatalf("mounts = %#v", capture.HostConfig.Mounts)
	}
	mask := capture.HostConfig.Mounts[1]
	if string(mask.Type) != "tmpfs" || mask.Source != "" || mask.Target != "/workspace/.cohotfs" || !mask.ReadOnly || mask.BindOptions != nil || mask.TmpfsOptions == nil || mask.TmpfsOptions.Mode.Perm() != 0o700 {
		t.Fatalf("unsafe Cohotfs state mask: %#v", mask)
	}
}

func TestCreateUsesExactEnabledResources(t *testing.T) {
	var capture capturedCreate
	adapter := testAdapter(t, &capture, `{}`)
	spec := validSpec()
	spec.Resources = runtime.ResourceLimits{Enabled: true, NanoCPUs: 8_000_000_000, MemoryBytes: 32 << 30, MemorySwapBytes: 64 << 30, PIDs: 4096, NofileSoft: 65536, NofileHard: 65536}
	if _, err := adapter.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	resources := capture.HostConfig.Resources
	if resources.NanoCPUs != spec.Resources.NanoCPUs || resources.Memory != spec.Resources.MemoryBytes || resources.MemorySwap != spec.Resources.MemorySwapBytes || resources.PidsLimit == nil || *resources.PidsLimit != 4096 || len(resources.Ulimits) != 1 || resources.Ulimits[0].Soft != 65536 || resources.Ulimits[0].Hard != 65536 {
		t.Fatalf("resources changed: %#v", resources)
	}
}

func TestCreateRefusesAbsentRuntimeBeforeCreate(t *testing.T) {
	var capture capturedCreate
	adapter := testAdapter(t, &capture, `{}`)
	spec := validSpec()
	spec.Runtime = "runsc"
	if _, err := adapter.Create(context.Background(), spec); err == nil || !runtime.IsUnsupported(err) {
		t.Fatalf("runtime error = %v", err)
	}
	if capture.Config.Image != "" {
		t.Fatal("created container after runtime capability failure")
	}
}

func TestCreateSelectsConfiguredRuntime(t *testing.T) {
	var capture capturedCreate
	adapter := testAdapter(t, &capture, `{"runsc":{"path":"runsc"}}`)
	spec := validSpec()
	spec.Runtime = "runsc"
	if _, err := adapter.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if capture.HostConfig == nil || capture.HostConfig.Runtime != "runsc" {
		t.Fatalf("runtime = %#v", capture.HostConfig)
	}
	if got := capture.HostConfig.Annotations["dev.gvisor.flag.host-uds"]; got != "create" {
		t.Fatalf("gVisor host UDS annotation = %q", got)
	}
}

func TestBuildReturnsMoreThanEventBufferWithoutBlocking(t *testing.T) {
	contextPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextPath, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var payload strings.Builder
	for index := range 20 {
		fmt.Fprintf(&payload, "{\"stream\":%q}\n", fmt.Sprintf("step %d\n", index))
	}
	imageID := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fmt.Fprintf(&payload, "{\"aux\":%q}\n", imageID)
	apiClient, err := client.New(
		client.WithHost("http://docker.test"),
		client.WithAPIVersion("1.55"),
		client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch {
			case request.URL.Path == "/v1.55/build":
				_, _ = io.Copy(io.Discard, request.Body)
				return jsonResponse(http.StatusOK, payload.String()), nil
			case strings.HasPrefix(request.URL.Path, "/v1.55/images/"):
				return jsonResponse(http.StatusOK, fmt.Sprintf(`{"Id":%q,"RepoDigests":["local@%s"],"Os":"linux","Architecture":"amd64"}`, imageID, imageID)), nil
			default:
				return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			}
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := newWithClient(apiClient, "unix:///var/run/docker.sock", "")
	image, events, err := adapter.Build(context.Background(), runtime.BuildRequest{
		Context: contextPath, Containerfile: "Containerfile", Tags: []string{"local:test"}, PermittedRoots: []string{contextPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for range events {
		count++
	}
	if count != 21 || image.Digest != imageID {
		t.Fatalf("build events=%d image=%#v", count, image)
	}
}

func TestImageIDFromBuildAuxAcceptsObjectAndString(t *testing.T) {
	imageID := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, raw := range []string{fmt.Sprintf(`{"ID":%q}`, imageID), strconv.Quote(imageID)} {
		if got := imageIDFromAux(json.RawMessage(raw)); got != imageID {
			t.Fatalf("image ID from %s = %q, want %q", raw, got, imageID)
		}
	}
	if got := imageIDFromAux(json.RawMessage(`{"digest":"unrelated"}`)); got != "" {
		t.Fatalf("unrelated auxiliary payload yielded image ID %q", got)
	}
}

func TestBuildRejectsContextOutsidePermittedRootsBeforeDocker(t *testing.T) {
	contextPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextPath, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var capture capturedCreate
	adapter := testAdapter(t, &capture, `{}`)
	if _, _, err := adapter.Build(context.Background(), runtime.BuildRequest{
		Context: contextPath, Containerfile: "Containerfile", PermittedRoots: []string{t.TempDir()},
	}); err == nil {
		t.Fatal("accepted build context outside permitted roots")
	}
}

func TestBuildReturnsClosedEventsOnPlanningFailure(t *testing.T) {
	_, events, err := (&Adapter{}).Build(context.Background(), runtime.BuildRequest{
		Context: filepath.Join(t.TempDir(), "missing"), PermittedRoots: []string{t.TempDir()},
	})
	if err == nil || events == nil {
		t.Fatalf("planning failure = %v, events = %v", err, events)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("planning failure emitted an unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("planning failure returned a blocking event channel")
	}
}

func TestResolveLocalInspectsWithoutRegistryPull(t *testing.T) {
	const reference = "ghcr.io/gosuda/cohotfs/workspace-base:dev"
	imageID := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var pulled bool
	apiClient, err := client.New(
		client.WithHost("http://docker.test"),
		client.WithAPIVersion("1.55"),
		client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if strings.Contains(request.URL.Path, "/images/create") {
				pulled = true
				return nil, fmt.Errorf("unexpected registry pull")
			}
			if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/images/") {
				return jsonResponse(http.StatusOK, fmt.Sprintf(`{"Id":%q,"Os":"linux","Architecture":"amd64"}`, imageID)), nil
			}
			return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := newWithClient(apiClient, "unix:///var/run/docker.sock", "")
	image, err := adapter.ResolveLocal(context.Background(), runtime.PullRequest{Reference: reference, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if pulled || image.Reference != reference || image.Digest != imageID {
		t.Fatalf("local resolution pulled=%t image=%#v", pulled, image)
	}
}
