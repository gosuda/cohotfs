//go:build linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime/docker"
	setupservice "github.com/gosuda/cohotfs/internal/setup"
	"github.com/gosuda/cohotfs/internal/state"
	workspaceservice "github.com/gosuda/cohotfs/internal/workspace"
	"github.com/spf13/cobra"
)

func buildSetupCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "setup"}
	validate := &cobra.Command{
		Use:  "validate",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			manifest := filepath.Join(cwd, ".cohotfs", "workspace.yaml")
			workspace, err := config.LoadWorkspace(manifest)
			if err != nil {
				return err
			}
			validation, err := setupservice.Validate(cwd, workspace.Spec.Setup, workspace.Spec.Image.Ref, "v1alpha1", os.Getuid(), os.Getgid())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "command: %v\nuser: %s\ntimeout: %s\ndigest: %s\n", validation.Command, validation.User, validation.Timeout, validation.Digest)
			return nil
		},
	}
	run := &cobra.Command{
		Use:  "run <workspace>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			return withWorkspaceRuntime(cmd.Context(), deps, func(_ *hostroot.Root, store *state.Store, backend *docker.Adapter, _ *workspaceservice.DockerService, _ config.HostConfig) error {
				record, err := resolveWorkspace(store, args[0])
				if err != nil {
					return err
				}
				planRaw, err := store.LoadWorkspaceArtifact(record.ID, "plan.json")
				if err != nil {
					return err
				}
				var plan workspaceservice.Plan
				if err := json.Unmarshal(planRaw, &plan); err != nil {
					return err
				}
				validation, err := setupservice.Validate(record.CanonicalSource, plan.Setup, record.ImageDigest, plan.Image.BootstrapAPI, record.OwnerUID, record.OwnerGID)
				if err != nil {
					return err
				}
				service := setupservice.NewService(store, backend)
				record, err = service.Run(cmd.Context(), record.ID, plan.Setup, validation, true, force)
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", record.ID, record.Status)
				return err
			})
		},
	}
	run.Flags().Bool("force", false, "rerun a successful setup")
	command.AddCommand(validate, run)
	return command
}
