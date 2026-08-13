//go:build linux

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gosuda/cohotfs/internal/apperr"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func projectConfigPath(root *hostroot.Root, source string) (string, string, error) {
	canonical, _, key, err := config.ProjectIdentity(source)
	if err != nil {
		return "", "", err
	}
	relative := filepath.Join("projects", key, "workspace.yaml")
	path, err := root.HostPath(relative)
	return canonical, path, err
}

func writeProjectDocument(root *hostroot.Root, source string, workspace config.Workspace) (string, error) {
	document, err := config.NewProjectDocument(source, workspace)
	if err != nil {
		return "", err
	}
	raw, err := config.Render(document)
	if err != nil {
		return "", err
	}
	_, _, key, err := config.ProjectIdentity(source)
	if err != nil {
		return "", err
	}
	directory := filepath.Join("projects", key)
	if err := root.EnsureDir(directory, 0o700); err != nil {
		return "", err
	}
	if err := root.AtomicWrite(filepath.Join(directory, "workspace.yaml"), raw, 0o600); err != nil {
		return "", err
	}
	return root.HostPath(filepath.Join(directory, "workspace.yaml"))
}

func loadProjectWorkspace(root *hostroot.Root, source string) (config.Workspace, string, error) {
	canonical, path, err := projectConfigPath(root, source)
	if err != nil {
		return config.Workspace{}, "", err
	}
	document, err := config.LoadProject(path, canonical)
	if err != nil {
		return config.Workspace{}, path, err
	}
	return document.Workspace, path, nil
}

func buildProjectConfigCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "project", Short: "Manage the trusted current-project configuration"}
	var edit bool
	editCommand := &cobra.Command{
		Use:  "edit",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return withRoot(deps, func(root *hostroot.Root) error {
				workspace, path, err := loadProjectWorkspace(root, cwd)
				if err != nil {
					return apperr.Wrap(apperr.ExitUsage, "project_config", err, "load project config: %v", err)
				}
				if edit {
					return editProjectConfigExternally(cmd, root, path, cwd)
				}
				if !terminalStreams(cmd.InOrStdin(), cmd.OutOrStdout()) {
					return apperr.New(apperr.ExitUsage, "terminal", "config project edit requires a terminal; use --edit with EDITOR")
				}
				updated, changed, err := runProjectConfigTUI(cmd, workspace)
				if err != nil || !changed {
					return err
				}
				_, err = writeProjectDocument(root, cwd, updated)
				return err
			})
		},
	}
	editCommand.Flags().BoolVar(&edit, "edit", false, "open the trusted config with $VISUAL or $EDITOR")
	show := &cobra.Command{
		Use:  "show",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return withRoot(deps, func(root *hostroot.Root) error {
				_, path, err := loadProjectWorkspace(root, cwd)
				if err != nil {
					return err
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			})
		},
	}
	command.AddCommand(editCommand, show)
	return command
}

func editProjectConfigExternally(cmd *cobra.Command, root *hostroot.Root, path, source string) error {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		for _, candidate := range []string{"vim", "vi", "nano"} {
			if found, err := exec.LookPath(candidate); err == nil {
				editor = found
				break
			}
		}
	}
	if editor == "" {
		return apperr.New(apperr.ExitUnavailable, "editor", "no editor found; set VISUAL or EDITOR")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".workspace.yaml.edit-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(original)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	parts := strings.Fields(editor)
	parts = append(parts, temporaryPath)
	process := exec.CommandContext(cmd.Context(), parts[0], parts[1:]...)
	process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return err
	}
	fd, err := unix.Open(temporaryPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	edited := os.NewFile(uintptr(fd), temporaryPath)
	info, statErr := edited.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		_ = edited.Close()
		if statErr != nil {
			return statErr
		}
		return fmt.Errorf("edited project config must be a regular file at most 1 MiB")
	}
	raw, readErr := io.ReadAll(io.LimitReader(edited, 1<<20+1))
	closeErr := edited.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if _, err := config.DecodeProject(raw, source); err != nil {
		return apperr.Wrap(apperr.ExitUsage, "project_config", err, "edited config is invalid: %v", err)
	}
	_, _, key, err := config.ProjectIdentity(source)
	if err != nil {
		return err
	}
	return root.AtomicWrite(filepath.Join("projects", key, "workspace.yaml"), raw, 0o600)
}

func terminalStreams(input io.Reader, output io.Writer) bool {
	in, inputOK := input.(*os.File)
	out, outputOK := output.(*os.File)
	return inputOK && outputOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

type projectConfigTUI struct {
	workspace config.Workspace
	cursor    int
	saved     bool
	quitting  bool
}

type projectToggle struct {
	name string
	get  func(*config.Workspace) bool
	set  func(*config.Workspace, bool)
}

func projectToggles() []projectToggle {
	return []projectToggle{
		{"OMP", func(w *config.Workspace) bool { return w.Spec.Integrations.Agents.OMP.Enabled }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Agents.OMP.Enabled = value }},
		{"OMP imports", func(w *config.Workspace) bool { return w.Spec.Integrations.Agents.OMP.Import.Enabled }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Agents.OMP.Import.Enabled = value }},
		{"OMP binary COW", func(w *config.Workspace) bool { return w.Spec.Integrations.Agents.OMP.Import.Binary }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Agents.OMP.Import.Binary = value }},
		{"OMP natives COW", func(w *config.Workspace) bool { return w.Spec.Integrations.Agents.OMP.Import.Natives }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Agents.OMP.Import.Natives = value }},
		{"OMP models COW", func(w *config.Workspace) bool { return w.Spec.Integrations.Agents.OMP.Import.Models }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Agents.OMP.Import.Models = value }},
		{"OMP config COW", func(w *config.Workspace) bool { return w.Spec.Integrations.Agents.OMP.Import.Config }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Agents.OMP.Import.Config = value }},
		{"Go toolchain", func(w *config.Workspace) bool { return w.Spec.Integrations.HostToolchains.Go.Enabled }, func(w *config.Workspace, value bool) {
			w.Spec.Integrations.HostToolchains.Go.Enabled = value
			w.Spec.Integrations.HostToolchains.Enabled = value || w.Spec.Integrations.HostToolchains.Rust.Enabled
		}},
		{"Rust toolchain", func(w *config.Workspace) bool { return w.Spec.Integrations.HostToolchains.Rust.Enabled }, func(w *config.Workspace, value bool) {
			w.Spec.Integrations.HostToolchains.Rust.Enabled = value
			w.Spec.Integrations.HostToolchains.Enabled = value || w.Spec.Integrations.HostToolchains.Go.Enabled
		}},
		{"Browser", func(w *config.Workspace) bool { return w.Spec.Integrations.Browser.Enabled }, func(w *config.Workspace, value bool) { w.Spec.Integrations.Browser.Enabled = value }},
		{"SSH agent", func(w *config.Workspace) bool { return w.Spec.Integrations.SSHAgent.Enabled }, func(w *config.Workspace, value bool) { w.Spec.Integrations.SSHAgent.Enabled = value }},
	}
}

func (model projectConfigTUI) Init() tea.Cmd { return nil }

func (model projectConfigTUI) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := message.(type) {
	case tea.KeyMsg:
		switch typed.String() {
		case "ctrl+c", "q", "esc":
			model.quitting = true
			return model, tea.Quit
		case "up", "k":
			if model.cursor > 0 {
				model.cursor--
			}
		case "down", "j":
			if model.cursor+1 < len(projectToggles()) {
				model.cursor++
			}
		case " ", "enter":
			item := projectToggles()[model.cursor]
			item.set(&model.workspace, !item.get(&model.workspace))
		case "s":
			if model.workspace.Spec.Integrations.Agents.OMP.Enabled && model.workspace.Spec.Integrations.Agents.OMP.Import.Enabled {
				model.workspace.Spec.Integrations.Agents.OMP.Import.Binary = true
			}
			if err := model.workspace.Validate(); err == nil {
				model.saved = true
				return model, tea.Quit
			}
		}
	}
	return model, nil
}

func (model projectConfigTUI) View() string {
	if model.saved {
		return "Saved trusted project configuration.\n"
	}
	if model.quitting {
		return "Configuration unchanged.\n"
	}
	var output strings.Builder
	output.WriteString("Cohotfs project settings\n\n")
	for index, item := range projectToggles() {
		cursor := "  "
		if index == model.cursor {
			cursor = "> "
		}
		mark := " "
		if item.get(&model.workspace) {
			mark = "x"
		}
		fmt.Fprintf(&output, "%s[%s] %s\n", cursor, mark, item.name)
	}
	output.WriteString("\n↑/↓ move  space toggle  s save  q cancel\n")
	return output.String()
}

func runProjectConfigTUI(cmd *cobra.Command, workspace config.Workspace) (config.Workspace, bool, error) {
	program := tea.NewProgram(projectConfigTUI{workspace: workspace}, tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.OutOrStdout()), tea.WithAltScreen())
	result, err := program.Run()
	if err != nil {
		return config.Workspace{}, false, err
	}
	model, ok := result.(projectConfigTUI)
	if !ok {
		return config.Workspace{}, false, errors.New("unexpected config TUI result")
	}
	return model.workspace, model.saved, nil
}
