package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/state"
	"github.com/spf13/cobra"
)

type runtimeSummary struct {
	Name         string   `json:"name"`
	Endpoint     string   `json:"endpoint"`
	Available    bool     `json:"available"`
	Capabilities []string `json:"capabilities"`
	Detail       string   `json:"detail,omitempty"`
}

func buildRuntimeCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "runtime"}
	for _, name := range []string{"list", "capabilities"} {
		leaf := &cobra.Command{
			Use:  name,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				format, _ := cmd.Flags().GetString("output")
				return withRoot(deps, func(root *hostroot.Root) error {
					path, _ := root.HostPath("config.yaml")
					host, err := config.LoadHost(path)
					if err != nil {
						return apperr.Wrap(apperr.ExitUsage, "config", err, "load host config: %v", err)
					}
					summaries := probeConfiguredRuntimes(host)
					if format == "json" {
						return writeJSON(cmd, summaries)
					}
					if format != "text" {
						return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
					}
					for _, summary := range summaries {
						status := "unavailable"
						if summary.Available {
							status = "available"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", summary.Name, status, summary.Endpoint)
					}
					return nil
				})
			},
		}
		leaf.Flags().String("output", "text", "output format: text or json")
		command.AddCommand(leaf)
	}
	return command
}

func probeConfiguredRuntimes(host config.HostConfig) []runtimeSummary {
	dockerEndpoint, err := resolveDockerEndpoint(host.Runtime.Docker.Endpoint)
	if err != nil {
		return []runtimeSummary{{Name: "docker", Endpoint: host.Runtime.Docker.Endpoint, Detail: err.Error()}}
	}
	return []runtimeSummary{
		probeUnixRuntime("docker", strings.TrimPrefix(dockerEndpoint, "unix://"), []string{"builder", "host_socket_bind", "interactive_exec", "runtime_selection"}, ""),
	}
}

func probeUnixRuntime(name, endpoint string, capabilities []string, detail string) runtimeSummary {
	summary := runtimeSummary{Name: name, Endpoint: endpoint, Capabilities: capabilities, Detail: detail}
	if endpoint == "" || !strings.HasPrefix(endpoint, "/") || strings.Contains(endpoint, "://") {
		if summary.Detail == "" {
			summary.Detail = "endpoint is not a local Unix socket"
		}
		return summary
	}
	info, err := os.Stat(endpoint)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		if summary.Detail == "" {
			summary.Detail = "Unix socket is absent or inaccessible"
		}
		return summary
	}
	connection, err := net.DialTimeout("unix", endpoint, 250*time.Millisecond)
	if err != nil {
		if summary.Detail == "" {
			summary.Detail = "Unix socket connection failed"
		}
		return summary
	}
	_ = connection.Close()
	summary.Available = true
	return summary
}

type workspaceSummary struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      state.Status `json:"status"`
	Backend     string       `json:"backend"`
	ImageDigest string       `json:"imageDigest"`
	Source      string       `json:"source"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
}

func summarizeWorkspace(workspace state.Workspace) workspaceSummary {
	return workspaceSummary{ID: workspace.ID, Name: workspace.Name, Status: workspace.Status, Backend: workspace.Backend, ImageDigest: workspace.ImageDigest, Source: workspace.CanonicalSource, CreatedAt: workspace.CreatedAt, UpdatedAt: workspace.UpdatedAt}
}

func buildWorkspaceCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "workspace"}
	command.AddCommand(buildWorkspaceLifecycleCommands(deps)...)
	list := &cobra.Command{
		Use:  "list",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, _ := cmd.Flags().GetString("output")
			return withStateStore(deps, func(store *state.Store) error {
				workspaces, err := store.ListWorkspaces()
				if err != nil {
					return err
				}
				summaries := make([]workspaceSummary, len(workspaces))
				for index, workspace := range workspaces {
					summaries[index] = summarizeWorkspace(workspace)
				}
				if format == "json" {
					return writeJSON(cmd, summaries)
				}
				if format != "text" {
					return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
				}
				for _, summary := range summaries {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", summary.ID, summary.Name, summary.Status, summary.Backend)
				}
				return nil
			})
		},
	}
	list.Flags().String("output", "text", "output format: text or json")
	status := &cobra.Command{
		Use:  "status <workspace>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, _ := cmd.Flags().GetString("output")
			return withStateStore(deps, func(store *state.Store) error {
				workspace, err := resolveWorkspace(store, args[0])
				if err != nil {
					return err
				}
				summary := summarizeWorkspace(workspace)
				if format == "json" {
					return writeJSON(cmd, summary)
				}
				if format != "text" {
					return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", summary.ID, summary.Name, summary.Status, summary.Backend)
				return nil
			})
		},
	}
	status.Flags().String("output", "text", "output format: text or json")
	command.AddCommand(list, status)
	return command
}

func resolveWorkspace(store *state.Store, nameOrID string) (state.Workspace, error) {
	if workspace, err := store.LoadWorkspace(nameOrID); err == nil {
		return workspace, nil
	}
	workspaces, err := store.ListWorkspaces()
	if err != nil {
		return state.Workspace{}, err
	}
	for _, workspace := range workspaces {
		if workspace.Name == nameOrID {
			return workspace, nil
		}
	}
	return state.Workspace{}, apperr.New(apperr.ExitStateConflict, "not_found", "workspace %q not found", nameOrID)
}

func withStateStore(deps Dependencies, fn func(*state.Store) error) error {
	return withRoot(deps, func(root *hostroot.Root) error {
		store, err := state.NewStore(root)
		if err != nil {
			return err
		}
		return fn(store)
	})
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
