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
	bindHost := "127.0.0.1"
	command := &cobra.Command{
		Use:   "port-forward <container-port>",
		Short: "Forward a host TCP port to the current workspace",
		Args:  cobra.ExactArgs(1),
	}
	workspaceName := addWorkspaceFlag(command)
	command.Flags().IntVar(&localPort, "local-port", 0, "host port (defaults to the container port)")
	command.Flags().StringVar(&bindHost, "host", bindHost, "host bind address: 127.0.0.1 or 0.0.0.0")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		containerPort, err := parsePort(args[0], "container port")
		if err != nil {
			return err
		}
		effectiveLocalPort := localPort
		if effectiveLocalPort == 0 {
			effectiveLocalPort = containerPort
		} else if _, err := parsePort(strconv.Itoa(effectiveLocalPort), "local port"); err != nil {
			return err
		}
		effectiveHost, err := parseBindHost(bindHost)
		if err != nil {
			return err
		}
		return withSSHWorkspace(deps, cmd, *workspaceName, func(record state.Workspace, root *hostroot.Root) error {
			if record.BootstrapAPI != containeragent.BootstrapAPI || !record.TCPForwarding {
				return apperr.New(apperr.ExitStateConflict, "state_conflict", "workspace %s does not support localhost forwarding; remove and recreate it", record.ID)
			}
			return runOpenSSHPortForward(cmd.Context(), cmd, root, record, effectiveHost, effectiveLocalPort, containerPort)
		})
	}
	return command
}

func parsePort(value, name string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, apperr.New(apperr.ExitUsage, "usage", "%s must be an integer from 1 to 65535", name)
	}
	return port, nil
}

func parseBindHost(value string) (string, error) {
	if value != "127.0.0.1" && value != "0.0.0.0" {
		return "", apperr.New(apperr.ExitUsage, "usage", "host must be 127.0.0.1 or 0.0.0.0")
	}
	return value, nil
}

func portForwardArguments(base []string, bindHost, workspaceID string, localPort, containerPort int) []string {
	forward := fmt.Sprintf("%s:%d:127.0.0.1:%d", bindHost, localPort, containerPort)
	arguments := append([]string(nil), base...)
	return append(arguments, "-T", "-N", "-o", "ExitOnForwardFailure=yes", "-L", forward, "agent@cohotfs-"+workspaceID)
}

func runOpenSSHPortForward(ctx context.Context, cmd *cobra.Command, root *hostroot.Root, record state.Workspace, bindHost string, localPort, containerPort int) error {
	sshPath, base, err := openSSHBaseArguments(root, record)
	if err != nil {
		return err
	}
	arguments := portForwardArguments(base, bindHost, record.ID, localPort, containerPort)
	fmt.Fprintf(cmd.ErrOrStderr(), "forwarding %s:%d -> %s:127.0.0.1:%d (Ctrl-C to stop)\n", bindHost, localPort, record.Name, containerPort)
	ssh := exec.CommandContext(ctx, sshPath, arguments...)
	ssh.Stdout = cmd.OutOrStdout()
	ssh.Stderr = cmd.ErrOrStderr()
	if err := ssh.Run(); ctx.Err() != nil {
		return nil
	} else {
		return err
	}
}
