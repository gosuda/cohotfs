package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkspacePrecedence(t *testing.T) {
	project := t.TempDir()
	manifestDir := filepath.Join(project, ".cohotfs")
	if err := os.Mkdir(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(manifestDir, "workspace.yaml")
	repository := `apiVersion: cohotfs.io/v1alpha1
kind: Workspace
metadata:
  name: repository
spec:
  runtime:
    isolation: gvisor
`
	if err := os.WriteFile(manifest, []byte(repository), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical, digest, _, err := ProjectIdentity(project)
	if err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(t.TempDir(), "override.yaml")
	overrideRaw := fmt.Sprintf(`apiVersion: cohotfs.io/v1alpha1
kind: ProjectOverride
sourcePath: %q
sourceDigest: %s
workspace:
  spec:
    runtime:
      isolation: standard
`, canonical, digest)
	if err := os.WriteFile(override, []byte(overrideRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	host := BuiltinHostConfig()
	host.Defaults.Runtime.Isolation = "standard"
	workspace, err := ResolveWorkspace(manifest, override, host, WorkspaceFlags{}, "example.invalid/base:dev")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Metadata.Name != "repository" || workspace.Spec.Runtime.Backend != "docker" || workspace.Spec.Runtime.Isolation != "standard" {
		t.Fatalf("resolved override workspace = %#v", workspace)
	}
	flagIsolation := "gvisor"
	workspace, err = ResolveWorkspace(manifest, override, host, WorkspaceFlags{Isolation: &flagIsolation}, "example.invalid/base:dev")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Metadata.Name != "repository" || workspace.Spec.Runtime.Backend != "docker" || workspace.Spec.Runtime.Isolation != "gvisor" {
		t.Fatalf("resolved flag workspace = %#v", workspace)
	}
}

func TestResolveWorkspaceRejectsUnknownMergedKey(t *testing.T) {
	project := t.TempDir()
	manifestDir := filepath.Join(project, ".cohotfs")
	if err := os.Mkdir(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(manifestDir, "workspace.yaml")
	raw := `apiVersion: cohotfs.io/v1alpha1
kind: Workspace
metadata:
  name: test
  privileged: true
`
	if err := os.WriteFile(manifest, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorkspace(manifest, "", BuiltinHostConfig(), WorkspaceFlags{}, "example.invalid/base:dev"); err == nil {
		t.Fatal("accepted unknown merged key")
	}
}

func TestResolveWorkspaceRejectsTypoBesideInheritedField(t *testing.T) {
	project := t.TempDir()
	manifestDir := filepath.Join(project, ".cohotfs")
	if err := os.Mkdir(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(manifestDir, "workspace.yaml")
	raw := `apiVersion: cohotfs.io/v1alpha1
kind: Workspace
metadata:
  name: test
spec:
  runtime:
    backen: unsupported
`
	if err := os.WriteFile(manifest, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorkspace(manifest, "", BuiltinHostConfig(), WorkspaceFlags{}, "example.invalid/base:dev"); err == nil {
		t.Fatal("accepted misspelled backend while retaining inherited backend")
	}
}

func TestOmittedIntegrationsRemainDisabled(t *testing.T) {
	project := t.TempDir()
	manifestDir := filepath.Join(project, ".cohotfs")
	if err := os.Mkdir(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(manifestDir, "workspace.yaml")
	raw := `apiVersion: cohotfs.io/v1alpha1
kind: Workspace
metadata:
  name: test
`
	if err := os.WriteFile(manifest, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := ResolveWorkspace(manifest, "", BuiltinHostConfig(), WorkspaceFlags{}, "example.invalid/base:dev")
	if err != nil {
		t.Fatal(err)
	}
	integrations := workspace.Spec.Integrations
	if integrations.HostToolchains.Enabled || integrations.Browser.Enabled || integrations.SSHAgent.Enabled || integrations.GitCredentials.Enabled || integrations.Agents.OMP.Enabled || integrations.Agents.Codex.Enabled || integrations.Agents.Claude.Enabled {
		t.Fatalf("omitted integrations enabled: %#v", integrations)
	}
}
