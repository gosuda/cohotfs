package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultWorkspaceAppliesHostDefaultsThenFlags(t *testing.T) {
	host := BuiltinHostConfig()
	host.Defaults.Runtime.Isolation = "standard"
	host.Defaults.Resources = ResourceSpec{
		Enabled:    true,
		CPU:        4,
		Memory:     8 << 30,
		MemorySwap: 9 << 30,
		PIDs:       1024,
		Nofile:     NofileLimit{Soft: 2048, Hard: 8192},
	}
	isolation := "gvisor"
	workspace, err := ResolveDefaultWorkspace("project", host, WorkspaceFlags{Isolation: &isolation}, "example.invalid/base:dev")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Spec.Runtime.Isolation != "gvisor" || workspace.Spec.Resources.CPU != 4 {
		t.Fatalf("resolved workspace = %#v", workspace)
	}
}

func TestProjectDocumentBindsConfigurationToCanonicalSource(t *testing.T) {
	source := t.TempDir()
	workspace := BuiltinWorkspace("project", "example.invalid/base:dev")
	document, err := NewProjectDocument(source, workspace)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Render(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeProject(raw, source)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SourcePath != document.SourcePath || decoded.SourceDigest != document.SourceDigest {
		t.Fatalf("decoded identity = %#v", decoded)
	}
	other := t.TempDir()
	if _, err := DecodeProject(raw, other); err == nil {
		t.Fatal("accepted project configuration for another source")
	}
}

func TestProjectDocumentRejectsUnknownFields(t *testing.T) {
	source := t.TempDir()
	workspace := BuiltinWorkspace("project", "example.invalid/base:dev")
	document, err := NewProjectDocument(source, workspace)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Render(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workspace.yaml")
	raw = append(raw, []byte("unexpected: true\n")...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProject(path, source); err == nil {
		t.Fatal("accepted unknown project configuration field")
	}
}
