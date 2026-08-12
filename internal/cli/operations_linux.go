//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/toolchain"
	"github.com/spf13/cobra"
)

type diagnosticCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type diagnosticsOutput struct {
	Checks []diagnosticCheck `json:"checks"`
}

func buildOnboardCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "onboard", Args: cobra.NoArgs}
	command.Flags().Bool("non-interactive", false, "report without mutation")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
		if nonInteractive {
			checks := collectDiagnostics(cmd.Context(), "")
			writeDiagnosticText(cmd, checks)
			return diagnosticFailure(checks)
		}
		return withRoot(deps, func(root *hostroot.Root) error {
			path, err := root.HostPath("config.yaml")
			if err != nil {
				return err
			}
			host, err := config.LoadHost(path)
			if err != nil {
				return apperr.Wrap(apperr.ExitUsage, "config", err, "load host config: %v", err)
			}
			candidates, err := toolchain.Discover(cmd.Context(), os.Environ())
			if err != nil {
				return err
			}
			selectSingleToolchains(&host, candidates)
			if host.Browser.LinuxExecutable == "" {
				host.Browser.LinuxExecutable = discoverExecutable("google-chrome", "google-chrome-stable", "chromium", "chromium-browser")
			}
			if err := host.Validate(); err != nil {
				return err
			}
			raw, err := config.Render(host)
			if err != nil {
				return err
			}
			if err := root.AtomicWrite("config.yaml", raw, 0o600); err != nil {
				return err
			}
			if err := ensureSSHClientKey(root); err != nil {
				return apperr.Wrap(apperr.ExitPolicy, "ssh_key", err, "prepare SSH client key: %v", err)
			}
			checks := collectDiagnostics(cmd.Context(), root.Path())
			writeDiagnosticText(cmd, checks)
			fmt.Fprintf(cmd.OutOrStdout(), "configured\t%s\n", path)
			return diagnosticFailure(checks)
		})
	}
	return command
}

func buildDoctorCommand() *cobra.Command {
	command := &cobra.Command{Use: "doctor", Args: cobra.NoArgs}
	command.Flags().String("output", "text", "output format: text or json")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		format, _ := cmd.Flags().GetString("output")
		checks := collectDiagnostics(cmd.Context(), "")
		if format == "json" {
			if err := writeJSON(cmd, diagnosticsOutput{Checks: checks}); err != nil {
				return err
			}
		} else if format == "text" {
			writeDiagnosticText(cmd, checks)
		} else {
			return apperr.New(apperr.ExitUsage, "usage", "output must be text or json")
		}
		return diagnosticFailure(checks)
	}
	return command
}

func collectDiagnostics(ctx context.Context, knownRoot string) []diagnosticCheck {
	checks := make([]diagnosticCheck, 0, 10)
	rootPath := knownRoot
	if rootPath == "" {
		if home, err := currentUserHomeDirectory(); err == nil {
			rootPath = filepath.Join(home, ".cohotfs")
		} else {
			checks = append(checks, diagnosticCheck{Name: "host-root", Status: "fail", Detail: err.Error(), Remediation: "repair the current user database entry"})
		}
	}
	host := config.BuiltinHostConfig()
	if rootPath != "" {
		rootCheck := inspectHostRoot(rootPath)
		checks = append(checks, rootCheck)
		if rootCheck.Status == "pass" {
			if raw, err := os.ReadFile(filepath.Join(rootPath, "config.yaml")); err == nil {
				if parsed, parseErr := config.DecodeHost(raw); parseErr == nil {
					host = parsed
				} else {
					checks = append(checks, diagnosticCheck{Name: "config", Status: "fail", Detail: parseErr.Error(), Remediation: "fix ~/.cohotfs/config.yaml"})
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				checks = append(checks, diagnosticCheck{Name: "config", Status: "fail", Detail: err.Error(), Remediation: "restore readable user-owned configuration"})
			}
		}
	}
	for _, summary := range probeConfiguredRuntimes(host) {
		status := "pass"
		remediation := ""
		if !summary.Available {
			status = "fail"
			remediation = "grant the current user access to a local Docker Engine Unix socket"
		}
		detail := summary.Endpoint
		if summary.Detail != "" {
			detail += ": " + summary.Detail
		}
		checks = append(checks, diagnosticCheck{Name: "runtime:" + summary.Name, Status: status, Detail: detail, Remediation: remediation})
	}
	checks = append(checks, executableCheck("openssh", "ssh", "install the OpenSSH client"))
	if path := discoverExecutable("fuse-overlayfs"); path != "" {
		checks = append(checks, diagnosticCheck{Name: "cache-overlay", Status: "pass", Detail: path})
	} else {
		checks = append(checks, diagnosticCheck{Name: "cache-overlay", Status: "warn", Detail: "fuse-overlayfs is unavailable; kernel unprivileged OverlayFS is probed when requested", Remediation: "install fuse-overlayfs or use isolated caches"})
	}
	if cwd, err := os.Getwd(); err == nil && strings.HasPrefix(filepath.Clean(cwd), "/mnt/") {
		checks = append(checks, diagnosticCheck{Name: "wsl-project-filesystem", Status: "warn", Detail: cwd, Remediation: "move the project to the WSL Linux filesystem for COW toolchains"})
	}
	candidates, err := toolchain.Discover(ctx, os.Environ())
	if err != nil {
		checks = append(checks, diagnosticCheck{Name: "toolchains", Status: "warn", Detail: err.Error()})
	} else {
		for _, candidate := range candidates {
			status := "pass"
			if !candidate.Compatible {
				status = "warn"
			}
			detail := candidate.Path + " " + candidate.Version
			if candidate.Reason != "" {
				detail += ": " + candidate.Reason
			}
			checks = append(checks, diagnosticCheck{Name: "toolchain:" + candidate.Kind, Status: status, Detail: detail})
		}
	}
	for _, agent := range []string{"omp", "codex", "claude"} {
		path := discoverExecutable(agent)
		status := "pass"
		if path == "" {
			status = "warn"
			path = "not found on PATH"
		}
		checks = append(checks, diagnosticCheck{Name: "agent:" + agent, Status: status, Detail: path})
	}
	return checks
}

func inspectHostRoot(path string) diagnosticCheck {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return diagnosticCheck{Name: "host-root", Status: "warn", Detail: path + " does not exist", Remediation: "run cohotfs onboard"}
	}
	if err != nil {
		return diagnosticCheck{Name: "host-root", Status: "fail", Detail: err.Error(), Remediation: "restore a user-owned mode 0700 directory"}
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || int(stat.Uid) != os.Getuid() {
		return diagnosticCheck{Name: "host-root", Status: "fail", Detail: fmt.Sprintf("unsafe identity or mode at %s", path), Remediation: "restore a non-symlink user-owned mode 0700 directory"}
	}
	return diagnosticCheck{Name: "host-root", Status: "pass", Detail: path}
}

func executableCheck(name, executable, remediation string) diagnosticCheck {
	path, err := exec.LookPath(executable)
	if err != nil {
		return diagnosticCheck{Name: name, Status: "fail", Detail: executable + " is unavailable", Remediation: remediation}
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return diagnosticCheck{Name: name, Status: "fail", Detail: err.Error(), Remediation: remediation}
	}
	return diagnosticCheck{Name: name, Status: "pass", Detail: canonical}
}

func discoverExecutable(names ...string) string {
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err == nil {
			return canonical
		}
	}
	return ""
}

func selectSingleToolchains(host *config.HostConfig, candidates []toolchain.Candidate) {
	byKind := map[string][]toolchain.Candidate{}
	for _, candidate := range candidates {
		if candidate.Compatible {
			byKind[candidate.Kind] = append(byKind[candidate.Kind], candidate)
		}
	}
	if len(byKind["go"]) == 1 {
		host.Toolchains.GoRoot = byKind["go"][0].Root
	}
	if len(byKind["rust"]) == 1 {
		host.Toolchains.RustToolchain = byKind["rust"][0].Root
	}
}

func writeDiagnosticText(cmd *cobra.Command, checks []diagnosticCheck) {
	for _, check := range checks {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s", check.Status, check.Name, check.Detail)
		if check.Remediation != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\t%s", check.Remediation)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
}

func diagnosticFailure(checks []diagnosticCheck) error {
	for _, check := range checks {
		if check.Status == "fail" {
			return apperr.New(apperr.ExitUnavailable, "doctor", "one or more required capabilities are unavailable")
		}
	}
	return nil
}
