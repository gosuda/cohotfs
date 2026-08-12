//go:build linux

package cli

import (
	"fmt"

	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/sshproxy"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/spf13/cobra"
)

func buildSSHProxyCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{
		Use:  "ssh-proxy",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, _ := cmd.Flags().GetString("workspace")
			return withRoot(deps, func(root *hostroot.Root) error {
				store, err := state.NewStore(root)
				if err != nil {
					return err
				}
				if err := sshproxy.Proxy(cmd.Context(), store, id, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
					return fmt.Errorf("SSH proxy: %w", err)
				}
				return nil
			})
		},
	}
	command.Flags().String("workspace", "", "workspace ID")
	_ = command.MarkFlagRequired("workspace")
	return command
}
