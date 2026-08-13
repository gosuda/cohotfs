// Package docker implements the Cohotfs lifecycle against a local Docker Engine.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/go-units"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const defaultPlatform = "linux/amd64"

type engineClient interface {
	Ping(context.Context, client.PingOptions) (client.PingResult, error)
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ImageBuild(context.Context, io.Reader, client.ImageBuildOptions) (client.ImageBuildResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
}

type Adapter struct {
	client   engineClient
	endpoint string
	gvisor   string
}

func New(endpoint, gvisorRuntime string) (*Adapter, error) {
	if endpoint == "" || endpoint == "auto" {
		endpoint = "unix:///var/run/docker.sock"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Path == "" {
		return nil, fmt.Errorf("Docker endpoint must be a local Unix socket")
	}
	apiClient, err := client.New(client.WithHost(endpoint))
	if err != nil {
		return nil, err
	}
	return &Adapter{client: apiClient, endpoint: endpoint, gvisor: gvisorRuntime}, nil
}

func newWithClient(apiClient engineClient, endpoint, gvisor string) *Adapter {
	return &Adapter{client: apiClient, endpoint: endpoint, gvisor: gvisor}
}

func (a *Adapter) Probe(ctx context.Context) (runtime.BackendInfo, error) {
	ping, err := a.client.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		return runtime.BackendInfo{Name: "docker", Endpoint: a.endpoint}, err
	}
	info, err := a.client.Info(ctx, client.InfoOptions{})
	if err != nil {
		return runtime.BackendInfo{Name: "docker", Endpoint: a.endpoint}, err
	}
	runtimes := make([]string, 0, len(info.Info.Runtimes))
	for name := range info.Info.Runtimes {
		runtimes = append(runtimes, name)
	}
	capabilities := map[runtime.Capability]bool{
		runtime.CapabilityInteractiveExec: true,
		runtime.CapabilityHostSocketBind:  true, runtime.CapabilityBuilder: true,
		runtime.CapabilityRuntimeSelect: true,
	}
	return runtime.BackendInfo{Name: "docker", Version: info.Info.ServerVersion + " api=" + ping.APIVersion, Endpoint: a.endpoint, Available: true, Capabilities: capabilities, Runtimes: runtimes}, nil
}

func (a *Adapter) Pull(ctx context.Context, request runtime.PullRequest) (runtime.ResolvedImage, error) {
	if err := validateImageRequest(request); err != nil {
		return runtime.ResolvedImage{}, err
	}
	response, err := a.client.ImagePull(ctx, request.Reference, client.ImagePullOptions{Platforms: []ocispec.Platform{{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		return runtime.ResolvedImage{}, err
	}
	defer response.Close()
	if err := consumePullEvents(response); err != nil {
		return runtime.ResolvedImage{}, err
	}
	return a.resolveImage(ctx, request.Reference)
}

// ResolveLocal resolves an image already present in the selected Docker Engine
// without contacting a registry.
func (a *Adapter) ResolveLocal(ctx context.Context, request runtime.PullRequest) (runtime.ResolvedImage, error) {
	if err := validateImageRequest(request); err != nil {
		return runtime.ResolvedImage{}, err
	}
	return a.resolveImage(ctx, request.Reference)
}

func validateImageRequest(request runtime.PullRequest) error {
	if request.Reference == "" {
		return fmt.Errorf("image reference is required")
	}
	platform := request.Platform
	if platform == "" {
		platform = defaultPlatform
	}
	if platform != defaultPlatform {
		return &runtime.UnsupportedError{Backend: "docker", Capability: runtime.Capability("platform"), Reason: "only linux/amd64 is supported"}
	}
	return nil
}

func consumePullEvents(reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	for {
		var event struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if event.Error != "" {
			return fmt.Errorf("image pull failed: %s", event.Error)
		}
	}
}

func (a *Adapter) resolveImage(ctx context.Context, reference string) (runtime.ResolvedImage, error) {
	inspect, err := a.client.ImageInspect(ctx, reference)
	if err != nil {
		return runtime.ResolvedImage{}, err
	}
	if inspect.Os != "linux" || inspect.Architecture != "amd64" {
		return runtime.ResolvedImage{}, fmt.Errorf("image_incompatible: platform is %s/%s", inspect.Os, inspect.Architecture)
	}
	digest := inspect.ID
	for _, candidate := range inspect.RepoDigests {
		if _, value, ok := strings.Cut(candidate, "@"); ok {
			digest = value
			break
		}
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return runtime.ResolvedImage{}, fmt.Errorf("image digest is unavailable")
	}
	return runtime.ResolvedImage{Reference: reference, Digest: digest, ResolvedAt: time.Now().UTC()}, nil
}

func (a *Adapter) Create(ctx context.Context, spec runtime.WorkspaceSpec) (runtime.WorkspaceRef, error) {
	if err := runtime.ValidateWorkspaceSpec(spec); err != nil {
		return runtime.WorkspaceRef{}, err
	}
	if spec.Runtime != "" {
		info, err := a.client.Info(ctx, client.InfoOptions{})
		if err != nil {
			return runtime.WorkspaceRef{}, err
		}
		if _, ok := info.Info.Runtimes[spec.Runtime]; !ok {
			return runtime.WorkspaceRef{}, &runtime.UnsupportedError{Backend: "docker", Capability: runtime.CapabilityRuntimeSelect, Reason: "configured runtime alias is absent"}
		}
	}
	containerConfig := &container.Config{
		Image: spec.Image.Digest, Cmd: spec.Command, Env: spec.Environment, Labels: runtime.CloneLabels(spec.Labels),
		User: "0:0", WorkingDir: "/", Tty: false,
	}
	hostConfig := &container.HostConfig{
		NetworkMode: "bridge", Privileged: false, SecurityOpt: []string{"no-new-privileges:true"}, Runtime: spec.Runtime,
	}
	if spec.GVisorHostUDS != "" {
		hostConfig.Annotations = map[string]string{"dev.gvisor.flag.host-uds": spec.GVisorHostUDS}
	}
	for _, declared := range spec.Mounts {
		configured := mount.Mount{Target: declared.Target, ReadOnly: declared.ReadOnly}
		switch declared.Type {
		case "bind":
			configured.Type = mount.TypeBind
			configured.Source = declared.Source
			configured.BindOptions = &mount.BindOptions{Propagation: mount.Propagation(declared.Propagation)}
		case "tmpfs":
			configured.Type = mount.TypeTmpfs
			configured.TmpfsOptions = &mount.TmpfsOptions{Mode: 0o700}
		default:
			return runtime.WorkspaceRef{}, fmt.Errorf("unsupported mount type %q", declared.Type)
		}
		hostConfig.Mounts = append(hostConfig.Mounts, configured)
	}
	if spec.Resources.Enabled {
		pids := spec.Resources.PIDs
		hostConfig.Resources = container.Resources{
			NanoCPUs: spec.Resources.NanoCPUs, Memory: spec.Resources.MemoryBytes, MemorySwap: spec.Resources.MemorySwapBytes, PidsLimit: &pids,
			Ulimits: []*units.Ulimit{{Name: "nofile", Soft: int64(spec.Resources.NofileSoft), Hard: int64(spec.Resources.NofileHard)}},
		}
	}
	name := "cohotfs-" + spec.WorkspaceID
	result, err := a.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: containerConfig, HostConfig: hostConfig, NetworkingConfig: &network.NetworkingConfig{},
		Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"}, Name: name,
	})
	if err != nil {
		if cerrdefs.IsConflict(err) {
			return a.resumeConflictingCreate(ctx, name, spec)
		}
		return runtime.WorkspaceRef{}, err
	}
	ref := runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": result.ID}, Nonce: spec.CreationNonce}
	if err := runtime.RecordWorkspaceRef(spec, ref); err != nil {
		_, cleanupErr := a.client.ContainerRemove(context.WithoutCancel(ctx), result.ID, client.ContainerRemoveOptions{Force: true})
		return runtime.WorkspaceRef{}, fmt.Errorf("record Docker container ID: %w (cleanup: %v)", err, cleanupErr)
	}
	return ref, nil
}

func (a *Adapter) resumeConflictingCreate(ctx context.Context, name string, spec runtime.WorkspaceSpec) (runtime.WorkspaceRef, error) {
	inspected, err := a.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return runtime.WorkspaceRef{}, fmt.Errorf("inspect conflicting Docker container: %w", err)
	}
	if inspected.Container.Config == nil || inspected.Container.ID == "" {
		return runtime.WorkspaceRef{}, fmt.Errorf("conflicting Docker container identity is incomplete")
	}
	for key, expected := range spec.Labels {
		if inspected.Container.Config.Labels[key] != expected {
			return runtime.WorkspaceRef{}, fmt.Errorf("conflicting Docker container label %s does not match", key)
		}
	}
	if inspected.Container.State != nil && inspected.Container.State.Running {
		return runtime.WorkspaceRef{}, fmt.Errorf("conflicting Docker container is unexpectedly running")
	}
	ref := runtime.WorkspaceRef{Backend: "docker", IDs: map[string]string{"container": inspected.Container.ID}, Nonce: spec.CreationNonce}
	if err := runtime.RecordWorkspaceRef(spec, ref); err != nil {
		return runtime.WorkspaceRef{}, fmt.Errorf("record recovered Docker container ID: %w", err)
	}
	return ref, nil
}

func (a *Adapter) Start(ctx context.Context, ref runtime.WorkspaceRef) error {
	id, err := containerID(ref)
	if err != nil {
		return err
	}
	_, err = a.client.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (a *Adapter) Inspect(ctx context.Context, ref runtime.WorkspaceRef) (runtime.WorkspaceStatus, error) {
	id, err := containerID(ref)
	if err != nil {
		return runtime.WorkspaceStatus{}, err
	}
	result, err := a.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return runtime.WorkspaceStatus{Exists: false}, nil
	}
	if err != nil {
		return runtime.WorkspaceStatus{}, err
	}
	status := runtime.WorkspaceStatus{Exists: true, Labels: runtime.CloneLabels(result.Container.Config.Labels)}
	if result.Container.State != nil {
		status.Running = result.Container.State.Running
		status.ExitCode = result.Container.State.ExitCode
	}
	if result.Container.Created != "" {
		status.CreationTime, _ = time.Parse(time.RFC3339Nano, result.Container.Created)
	}
	return status, nil
}

func (a *Adapter) Stop(ctx context.Context, ref runtime.WorkspaceRef, timeout time.Duration) error {
	id, err := containerID(ref)
	if err != nil {
		return err
	}
	seconds := int(math.Ceil(timeout.Seconds()))
	_, err = a.client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &seconds})
	if cerrdefs.IsNotFound(err) || cerrdefs.IsNotModified(err) {
		return nil
	}
	return err
}

func (a *Adapter) Delete(ctx context.Context, ref runtime.WorkspaceRef) error {
	id, err := containerID(ref)
	if err != nil {
		return err
	}
	_, err = a.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: false, RemoveVolumes: false})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (a *Adapter) ExecSync(ctx context.Context, ref runtime.WorkspaceRef, request runtime.ExecRequest) (runtime.ExecResult, error) {
	id, err := containerID(ref)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	if len(request.Argv) == 0 {
		return runtime.ExecResult{}, fmt.Errorf("exec argv is required")
	}
	if request.OutputLimit <= 0 {
		request.OutputLimit = 1 << 20
	}
	execContext := ctx
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		execContext, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}
	created, err := a.client.ExecCreate(execContext, id, client.ExecCreateOptions{User: request.User, Cmd: request.Argv, Env: request.Environment, WorkingDir: request.WorkingDir, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return runtime.ExecResult{}, err
	}
	attached, err := a.client.ExecAttach(execContext, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return runtime.ExecResult{}, err
	}
	defer attached.Close()
	limited := &limitBuffer{limit: request.OutputLimit}
	_, copyErr := stdcopy.StdCopy(limited, limited, attached.Reader)
	if copyErr != nil && execContext.Err() == nil {
		return runtime.ExecResult{}, copyErr
	}
	inspected, inspectErr := a.client.ExecInspect(context.WithoutCancel(ctx), created.ID, client.ExecInspectOptions{})
	if inspectErr != nil {
		return runtime.ExecResult{}, inspectErr
	}
	if execContext.Err() != nil {
		return runtime.ExecResult{ExitCode: inspected.ExitCode, Output: limited.Bytes(), Truncated: limited.truncated}, execContext.Err()
	}
	return runtime.ExecResult{ExitCode: inspected.ExitCode, Output: limited.Bytes(), Truncated: limited.truncated}, nil
}

func (a *Adapter) ExecInteractive(ctx context.Context, ref runtime.WorkspaceRef, request runtime.InteractiveRequest) error {
	id, err := containerID(ref)
	if err != nil {
		return err
	}
	created, err := a.client.ExecCreate(ctx, id, client.ExecCreateOptions{User: request.User, Cmd: request.Argv, Env: request.Environment, WorkingDir: request.WorkingDir, TTY: request.Terminal, AttachStdin: true, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return err
	}
	attached, err := a.client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: request.Terminal})
	if err != nil {
		return err
	}
	defer attached.Close()
	writeDone := make(chan struct{})
	go func() {
		if request.Stdin != nil {
			_, _ = io.Copy(attached.Conn, request.Stdin)
		}
		_ = attached.CloseWrite()
		close(writeDone)
	}()
	if request.Terminal {
		_, err = io.Copy(request.Stdout, attached.Reader)
	} else {
		_, err = stdcopy.StdCopy(request.Stdout, request.Stderr, attached.Reader)
	}
	<-writeDone
	return err
}

func containerID(ref runtime.WorkspaceRef) (string, error) {
	if ref.Backend != "docker" || ref.IDs["container"] == "" || ref.Nonce == "" {
		return "", fmt.Errorf("invalid Docker workspace reference")
	}
	return ref.IDs["container"], nil
}

type limitBuffer struct {
	bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitBuffer) Write(data []byte) (int, error) {
	if int64(b.Len()) >= b.limit {
		b.truncated = true
		return len(data), nil
	}
	remaining := b.limit - int64(b.Len())
	write := data
	if int64(len(write)) > remaining {
		write = write[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(write)
	return len(data), nil
}
