package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/moby/moby/client"
)

func TestImageResolveDoesNotAssertCompatibility(t *testing.T) {
	apiClient, err := client.New(
		client.WithHost("http://docker.test"), client.WithAPIVersion("1.55"),
		client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1.55/images/example/json" {
				return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
			}
			return jsonResponse(http.StatusOK, `{"Id":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","RepoDigests":["example@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"],"Os":"linux","Architecture":"amd64"}`), nil
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := newWithClient(apiClient, "test", "")
	image, err := adapter.resolveImage(context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if image.BootstrapAPI != "" {
		t.Fatalf("unprobed image asserted compatibility: %#v", image)
	}
}

func TestCompatibilityProbeMarksOnlySuccessfulCheck(t *testing.T) {
	var removed bool
	apiClient, err := client.New(
		client.WithHost("http://docker.test"), client.WithAPIVersion("1.55"),
		client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/v1.55/containers/create":
				return jsonResponse(http.StatusCreated, `{"Id":"check-id"}`), nil
			case request.Method == http.MethodPost && request.URL.Path == "/v1.55/containers/check-id/start":
				return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			case request.Method == http.MethodPost && request.URL.Path == "/v1.55/containers/check-id/wait":
				return jsonResponse(http.StatusOK, `{"StatusCode":0}`), nil
			case request.Method == http.MethodDelete && request.URL.Path == "/v1.55/containers/check-id":
				removed = true
				return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
			default:
				return nil, fmt.Errorf("unexpected %s %s", request.Method, request.URL.Path)
			}
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	adapter := newWithClient(apiClient, "test", "")
	image, err := adapter.CheckCompatibility(context.Background(), runtime.ResolvedImage{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if image.BootstrapAPI != "v1alpha1" || !removed {
		t.Fatalf("probe result = %#v removed=%v", image, removed)
	}
}
