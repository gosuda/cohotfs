package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gosuda/cohotfs/internal/buildcontext"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Build plans the complete context before calling Docker, then streams the
// identity-checked tar through an io.Pipe. The caller receives a stable event
// channel and the resolved image only after the Engine reports success.
func (a *Adapter) Build(ctx context.Context, request runtime.BuildRequest) (runtime.ResolvedImage, <-chan runtime.BuildEvent, error) {
	plan, err := buildcontext.BuildPlan(request.Context, buildcontext.Options{PermittedRoots: request.PermittedRoots, Containerfile: request.Containerfile, CohotfsRoot: request.CohotfsRoot})
	if err != nil {
		return runtime.ResolvedImage{}, closedBuildEvents(), err
	}
	reader, writer := io.Pipe()
	go func() { _ = writer.CloseWithError(plan.WriteTar(writer)) }()
	buildArgs := make(map[string]*string, len(request.Args))
	for key, value := range request.Args {
		copy := value
		buildArgs[key] = &copy
	}
	result, err := a.client.ImageBuild(ctx, reader, client.ImageBuildOptions{
		Tags: request.Tags, Dockerfile: request.Containerfile, Target: request.Target,
		BuildArgs: buildArgs, Remove: true, ForceRemove: true, Version: build.BuilderBuildKit,
		Platforms: []ocispec.Platform{{OS: "linux", Architecture: "amd64"}},
	})
	if err != nil {
		_ = reader.Close()
		return runtime.ResolvedImage{}, closedBuildEvents(), err
	}
	var collected []runtime.BuildEvent
	var imageID string
	decodeErr := func() error {
		defer result.Body.Close()
		decoder := json.NewDecoder(result.Body)
		for {
			var message struct {
				Stream string          `json:"stream"`
				Error  string          `json:"error"`
				Aux    json.RawMessage `json:"aux"`
			}
			if err := decoder.Decode(&message); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			if candidate := imageIDFromAux(message.Aux); candidate != "" {
				imageID = candidate
			}
			event := runtime.BuildEvent{Time: time.Now().UTC(), Stream: "build", Message: message.Stream, Error: message.Error}
			collected = append(collected, event)
			if message.Error != "" {
				return fmt.Errorf("image build failed: %s", message.Error)
			}
		}
	}()
	events := make(chan runtime.BuildEvent, len(collected))
	for _, event := range collected {
		events <- event
	}
	close(events)
	if decodeErr != nil {
		return runtime.ResolvedImage{}, events, decodeErr
	}
	if imageID == "" && len(request.Tags) != 0 {
		imageID = request.Tags[0]
	}
	if imageID == "" {
		return runtime.ResolvedImage{}, events, fmt.Errorf("build result image ID is unavailable")
	}
	image, err := a.resolveImage(ctx, imageID)
	if err != nil {
		return runtime.ResolvedImage{}, events, err
	}
	return image, events, nil
}

func imageIDFromAux(aux json.RawMessage) string {
	var result struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(aux, &result); err == nil && result.ID != "" {
		return result.ID
	}
	var imageID string
	if err := json.Unmarshal(aux, &imageID); err == nil {
		return imageID
	}
	return ""
}

func closedBuildEvents() <-chan runtime.BuildEvent {
	events := make(chan runtime.BuildEvent)
	close(events)
	return events
}
