//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/runtime/docker"
	"github.com/gosuda/cohotfs/internal/state"
	workspaceservice "github.com/gosuda/cohotfs/internal/workspace"
	"github.com/spf13/cobra"
)

func buildImageCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "image"}
	pull := &cobra.Command{
		Use:  "pull <reference>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withWorkspaceRuntime(cmd.Context(), deps, func(_ *hostroot.Root, _ *state.Store, backend *docker.Adapter, _ *workspaceservice.DockerService, _ config.HostConfig) error {
				image, err := backend.Pull(cmd.Context(), runtimePullRequest(args[0]))
				if err != nil {
					return apperr.Wrap(apperr.ExitRuntime, "image_pull", err, "pull image: %v", err)
				}
				image, err = backend.CheckCompatibility(cmd.Context(), image)
				if err != nil {
					return apperr.Wrap(apperr.ExitRuntime, "image_incompatible", err, "%v", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), image.Digest)
				return nil
			})
		},
	}
	build := &cobra.Command{
		Use:  "build",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return withWorkspaceRuntime(cmd.Context(), deps, func(root *hostroot.Root, _ *state.Store, backend *docker.Adapter, _ *workspaceservice.DockerService, host config.HostConfig) error {
				prepared, err := prepareWorkspaceDirectory(root, cwd, host, false)
				if err != nil {
					return err
				}
				spec := prepared.workspace.Spec.Image.Build
				if spec == nil {
					return apperr.New(apperr.ExitUsage, "image_build", "workspace manifest does not declare spec.image.build")
				}
				contextPath, err := workspaceservice.CanonicalSource(filepath.Join(cwd, spec.Context))
				if err != nil {
					return err
				}
				tag := "cohotfs/" + strings.ToLower(prepared.workspace.Metadata.Name) + ":local"
				image, events, err := backend.Build(cmd.Context(), runtime.BuildRequest{
					Context: contextPath, Containerfile: spec.Containerfile, Target: spec.Target,
					Args: spec.Args, Tags: []string{tag}, PermittedRoots: host.PermittedRoots, CohotfsRoot: root.Path(),
				})
				for event := range events {
					if event.Message != "" {
						fmt.Fprint(cmd.ErrOrStderr(), event.Message)
					}
					if event.Error != "" {
						fmt.Fprintln(cmd.ErrOrStderr(), event.Error)
					}
				}
				if err != nil {
					return apperr.Wrap(apperr.ExitRuntime, "image_build", err, "build image: %v", err)
				}
				image, err = backend.CheckCompatibility(cmd.Context(), image)
				if err != nil {
					return apperr.Wrap(apperr.ExitRuntime, "image_incompatible", err, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", tag, image.Digest)
				return nil
			})
		},
	}
	command.AddCommand(pull, build)
	return command
}
