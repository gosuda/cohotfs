//go:build linux

package cli

import (
	"fmt"
	"strings"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/hostservice"
	"github.com/spf13/cobra"
)

type hostStatusOutput struct {
	Running bool `json:"running"`
	api.HostStatus
}

func buildHostCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "host"}
	status := &cobra.Command{
		Use:  "status",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, _ := cmd.Flags().GetString("output")
			return withRoot(deps, func(root *hostroot.Root) error {
				client, err := hostservice.NewClient(root)
				if err != nil {
					return err
				}
				current, err := client.Status(cmd.Context())
				if err != nil {
					if hostservice.IsUnavailable(err) {
						if format == "json" {
							return writeJSON(cmd, hostStatusOutput{})
						}
						if format != "text" {
							return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
						}
						fmt.Fprintln(cmd.OutOrStdout(), "stopped")
						return nil
					}
					return apperr.Wrap(apperr.ExitPolicy, "host_identity", err, "host service validation failed: %v", err)
				}
				output := hostStatusOutput{Running: true, HostStatus: current}
				if format == "json" {
					return writeJSON(cmd, output)
				}
				if format != "text" {
					return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "running\tpid=%d\tleases=%d\n", current.PID, len(current.Leases))
				return nil
			})
		},
	}
	status.Flags().String("output", "text", "output format: text or json")
	stop := &cobra.Command{
		Use:  "stop",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			force, _ := cmd.Flags().GetBool("yes")
			return withRoot(deps, func(root *hostroot.Root) error {
				client, err := hostservice.NewClient(root)
				if err != nil {
					return err
				}
				if err := client.Stop(cmd.Context(), force); err != nil {
					if hostservice.IsUnavailable(err) {
						fmt.Fprintln(cmd.OutOrStdout(), "already stopped")
						return nil
					}
					if strings.Contains(err.Error(), "leases_active") {
						return apperr.Wrap(apperr.ExitStateConflict, "leases_active", err, "%v", err)
					}
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "stopped")
				return nil
			})
		},
	}
	stop.Flags().Bool("yes", false, "drain active leases")
	serve := &cobra.Command{
		Use:    "serve",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRoot(deps, func(root *hostroot.Root) error {
				server, err := hostservice.NewServer(root)
				if err != nil {
					return err
				}
				return server.Serve(cmd.Context())
			})
		},
	}
	command.AddCommand(status, stop, serve)
	return command
}
