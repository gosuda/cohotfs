// Package cli owns the deterministic Cohotfs command tree and exit mapping.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/spf13/cobra"
)

var Version = "dev"

func defaultImageReference() string {
	return "ghcr.io/gosuda/cohotfs/workspace-base:" + Version
}

func defaultImagePullPolicy() string {
	if Version == "dev" {
		return config.ImagePullNever
	}
	return config.ImagePullAlways
}

type Dependencies struct {
	OpenRoot func() (*hostroot.Root, error)
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer, deps Dependencies) int {
	command := NewRootCommand(deps)
	command.SetArgs(args)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetContext(ctx)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return apperr.Code(err)
	}
	return apperr.ExitSuccess
}

func NewRootCommand(deps Dependencies) *cobra.Command {
	if deps.OpenRoot == nil {
		deps.OpenRoot = hostroot.Open
	}
	allowHome := false
	root := &cobra.Command{
		Use:           "cohotfs",
		Version:       Version,
		Short:         "Isolated, host-integrated workspaces for agentic AI",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			home, err := currentUserHomeDirectory()
			if err != nil {
				return apperr.Wrap(apperr.ExitPolicy, "home_directory", err, "resolve current user home: %v", err)
			}
			canonical, homeMount, err := validateBareDirectory(cwd, home, allowHome)
			if err != nil {
				return err
			}
			return runBareWorkspace(deps, cmd, canonical, homeMount)
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return apperr.Wrap(apperr.ExitUsage, "usage", err, "%v", err)
	})
	root.CompletionOptions.DisableDefaultCmd = true
	root.Flags().BoolVar(&allowHome, "allow-home", false, "allow the bare command to mount the current user's home directory")
	root.AddCommand(
		newInitCommand(deps),
		buildOnboardCommand(deps),
		buildDoctorCommand(),
		newConfigCommand(deps),
		newRuntimeCommand(deps),
		newWorkspaceCommand(deps),
		buildImageCommand(deps),
		newSetupCommand(deps),
		buildShellCommand(deps),
		buildExecCommand(deps),
		buildSSHProxyCommand(deps),
		buildAgentCommand(deps),
		newHostCommand(deps),
	)
	return root
}

func currentUserHomeDirectory() (string, error) {
	current, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(current.HomeDir) {
		return "", fmt.Errorf("current user home is not absolute")
	}
	return current.HomeDir, nil
}

func validateBareDirectory(cwd, home string, allowHome bool) (string, bool, error) {
	canonical, _, _, err := config.ProjectIdentity(cwd)
	if err != nil {
		return "", false, err
	}
	canonicalHome, _, _, err := config.ProjectIdentity(home)
	if err != nil {
		return "", false, err
	}
	isHome := canonical == canonicalHome
	if isHome && !allowHome {
		return "", false, apperr.New(apperr.ExitPolicy, "home_directory", "refusing to mount the home directory; rerun with cohotfs --allow-home to grant it explicitly")
	}
	return canonical, isHome, nil
}

func newRuntimeCommand(deps Dependencies) *cobra.Command {
	return buildRuntimeCommand(deps)
}

func newWorkspaceCommand(deps Dependencies) *cobra.Command {
	return buildWorkspaceCommand(deps)
}

func newSetupCommand(deps Dependencies) *cobra.Command {
	return buildSetupCommand(deps)
}

func newHostCommand(deps Dependencies) *cobra.Command {
	return buildHostCommand(deps)
}

func newConfigCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "config"}
	show := &cobra.Command{
		Use:  "show",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRoot(deps, func(root *hostroot.Root) error {
				path, _ := root.HostPath("config.yaml")
				host, err := config.LoadHost(path)
				if err != nil {
					return apperr.Wrap(apperr.ExitUsage, "config", err, "load host config: %v", err)
				}
				data, err := config.Render(host)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err
			})
		},
	}
	validate := &cobra.Command{
		Use:  "validate",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withRoot(deps, func(root *hostroot.Root) error {
				path, _ := root.HostPath("config.yaml")
				if _, err := config.LoadHost(path); err != nil {
					return apperr.Wrap(apperr.ExitUsage, "config", err, "invalid host config: %v", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "valid")
				return nil
			})
		},
	}
	command.AddCommand(show, validate)
	return command
}

var invalidName = regexp.MustCompile(`[^a-z0-9._-]+`)

func newInitCommand(deps Dependencies) *cobra.Command {
	return &cobra.Command{
		Use:  "init",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			canonical, digest, key, err := config.ProjectIdentity(cwd)
			if err != nil {
				return apperr.Wrap(apperr.ExitUsage, "project", err, "resolve project: %v", err)
			}
			name := strings.ToLower(filepath.Base(canonical))
			name = strings.Trim(invalidName.ReplaceAllString(name, "-"), "-._")
			if name == "" {
				name = "workspace"
			}
			workspace := config.BuiltinWorkspace(name, defaultImageReference())
			workspace.Spec.Image.PullPolicy = defaultImagePullPolicy()
			data, err := config.Render(workspace)
			if err != nil {
				return err
			}
			manifestDir := filepath.Join(canonical, ".cohotfs")
			manifestPath := filepath.Join(manifestDir, "workspace.yaml")
			if _, err := os.Lstat(manifestPath); err == nil {
				return apperr.New(apperr.ExitStateConflict, "state_conflict", "%s already exists", manifestPath)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Mkdir(manifestDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			file, err := os.OpenFile(manifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			if _, err = file.Write(data); err == nil {
				err = file.Sync()
			}
			closeErr := file.Close()
			if err != nil {
				_ = os.Remove(manifestPath)
				return err
			}
			if closeErr != nil {
				_ = os.Remove(manifestPath)
				return closeErr
			}
			if err := withRoot(deps, func(root *hostroot.Root) error {
				dir := "projects/" + key
				if err := root.EnsureDir(dir, 0o700); err != nil {
					return err
				}
				override := map[string]any{"apiVersion": config.APIVersion, "kind": "ProjectOverride", "sourcePath": canonical, "sourceDigest": digest}
				raw, err := json.MarshalIndent(override, "", "  ")
				if err != nil {
					return err
				}
				return root.AtomicWrite(dir+"/override.yaml", append(raw, '\n'), 0o600)
			}); err != nil {
				_ = os.Remove(manifestPath)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s\nrun: cohotfs workspace create\n", manifestPath)
			return nil
		},
	}
}

func withRoot(deps Dependencies, fn func(*hostroot.Root) error) error {
	root, err := deps.OpenRoot()
	if err != nil {
		return apperr.Wrap(apperr.ExitPolicy, "host_root", err, "open Cohotfs root: %v", err)
	}
	defer root.Close()
	return fn(root)
}
