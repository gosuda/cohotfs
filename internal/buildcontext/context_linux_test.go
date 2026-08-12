//go:build linux

package buildcontext

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildPlanExcludesSecretsAndHonorsIgnore(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"Containerfile": "FROM scratch", "main.go": "package main", ".env": "TOKEN=secret",
		"auth.json": "secret", ".dockerignore": "ignored/*\n!ignored/keep.txt\n", "ignored/drop.txt": "drop", "ignored/keep.txt": "keep",
	}
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := BuildPlan(root, Options{PermittedRoots: []string{root}, Containerfile: "Containerfile"})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, entry := range plan.Entries {
		paths[entry.Path] = true
	}
	if paths[".env"] || paths["auth.json"] || paths["ignored/drop.txt"] || !paths["ignored/keep.txt"] || !paths["Containerfile"] || !paths[".dockerignore"] {
		t.Fatalf("planned paths = %v", paths)
	}
	var buffer bytes.Buffer
	if err := plan.WriteTar(&buffer); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&buffer)
	var tarPaths []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		tarPaths = append(tarPaths, header.Name)
	}
	if len(tarPaths) != len(plan.Entries) {
		t.Fatalf("tar entries = %v, plan = %#v", tarPaths, plan.Entries)
	}
}

func TestBuildPlanRejectsEscapesSpecialFilesAndLimits(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(root, Options{PermittedRoots: []string{root}}); err == nil {
		t.Fatal("accepted escaping symlink")
	}
	if err := os.Remove(filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(root, Options{PermittedRoots: []string{root}, MaxBytes: 16}); err == nil {
		t.Fatal("accepted oversized context")
	}
	if _, err := BuildPlan(root, Options{PermittedRoots: []string{t.TempDir()}}); err == nil {
		t.Fatal("accepted context outside permitted roots")
	}
}

func TestWriteTarRejectsChangedIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(root, Options{PermittedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteTar(io.Discard); err == nil {
		t.Fatal("streamed replaced build context entry")
	}
}
