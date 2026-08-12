//go:build linux

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/spf13/cobra"
)

func buildPortForwardCommand(deps Dependencies) *cobra.Command {
	localPort := 0
	command := &cobra.Command{
		Use:   "port-forward <workspace> <container-port>",
		Short: "Forward a host loopback port to a workspace loopback port",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			containerPort, err := parsePort(args[1], "container port")
			if err != nil {
				return err
			}
			effectiveLocalPort := localPort
			if effectiveLocalPort == 0 {
				effectiveLocalPort = containerPort
			} else if _, err := parsePort(strconv.Itoa(effectiveLocalPort), "local port"); err != nil {
				return err
			}
			return withSSHWorkspace(deps, cmd, args[0], func(record state.Workspace, root *hostroot.Root) error {
				if record.BootstrapAPI != containeragent.BootstrapAPI || !record.TCPForwarding {
					return apperr.New(apperr.ExitStateConflict, "state_conflict", "workspace %s does not support localhost forwarding; remove and recreate it", record.ID)
				}
				return runOpenSSHPortForward(cmd.Context(), cmd, root, record, effectiveLocalPort, containerPort)
			})
		},
	}
	command.Flags().IntVar(&localPort, "local-port", 0, "host loopback port (defaults to the container port)")
	return command
}

func parsePort(value, name string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, apperr.New(apperr.ExitUsage, "usage", "%s must be an integer from 1 to 65535", name)
	}
	return port, nil
}

func portForwardArguments(base []string, workspaceID string, localPort, containerPort int) []string {
	forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, containerPort)
	arguments := append([]string(nil), base...)
	return append(arguments, "-T", "-N", "-o", "ExitOnForwardFailure=yes", "-L", forward, "agent@cohotfs-"+workspaceID)
}

func runOpenSSHPortForward(ctx context.Context, cmd *cobra.Command, root *hostroot.Root, record state.Workspace, localPort, containerPort int) error {
	sshPath, base, err := openSSHBaseArguments(root, record)
	if err != nil {
		return err
	}
	arguments := portForwardArguments(base, record.ID, localPort, containerPort)
	fmt.Fprintf(cmd.ErrOrStderr(), "forwarding 127.0.0.1:%d -> %s:127.0.0.1:%d (Ctrl-C to stop)\n", localPort, record.Name, containerPort)
	ssh := exec.CommandContext(ctx, sshPath, arguments...)
	ssh.Stdout = cmd.OutOrStdout()
	ssh.Stderr = cmd.ErrOrStderr()
	if err := ssh.Run(); ctx.Err() != nil {
		return nil
	} else {
		return err
	}
}
