package docker

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// CheckCompatibility creates a disposable container with no host source or
// integration mounts and runs the image's own Cohotfs agent check. It always
// removes the check object and marks BootstrapAPI only after an exact success.
func (a *Adapter) CheckCompatibility(ctx context.Context, image runtime.ResolvedImage) (runtime.ResolvedImage, error) {
	if image.Digest == "" {
		return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: image digest is required")
	}
	created, err := a.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      image.Digest,
			Entrypoint: []string{"/usr/local/libexec/cohotfs-agent"},
			Cmd:        []string{"check", "--bootstrap-api", "v1alpha1"},
			User:       "0:0", AttachStdout: true, AttachStderr: true,
			Labels: map[string]string{"io.cohotfs.check": "image-compatibility"},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none", Privileged: false, SecurityOpt: []string{"no-new-privileges:true"}, AutoRemove: false},
		Platform:   &ocispec.Platform{OS: "linux", Architecture: "amd64"},
	})
	if err != nil {
		return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: create check container: %w", err)
	}
	defer a.client.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
	if _, err := a.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: start check container: %w", err)
	}
	wait := a.client.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		if err != nil {
			return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: wait for check: %w", err)
		}
	case result := <-wait.Result:
		if result.Error != nil || result.StatusCode != 0 {
			logs, _ := a.client.ContainerLogs(context.WithoutCancel(ctx), created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
			var output bytes.Buffer
			if logs != nil {
				_, _ = output.ReadFrom(logs)
				_ = logs.Close()
			}
			return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: check exited %d: %s", result.StatusCode, strings.TrimSpace(output.String()))
		}
	case <-time.After(30 * time.Second):
		return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: check timed out")
	}
	image.BootstrapAPI = "v1alpha1"
	return image, nil
}
