//go:build linux

package hostroot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenForTestCreatesExactLayoutAndModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root")
	root, err := OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, rel := range append([]string{"."}, layout...) {
		info, err := os.Stat(filepath.Join(path, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode/type = %v, want directory 0700", rel, info.Mode())
		}
	}
	if err := root.AtomicWrite("state/images/test.json", []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile("state/images/test.json")
	if err != nil || string(data) != "ok" {
		t.Fatalf("round trip = %q, %v", data, err)
	}
	if err := root.AtomicWrite("config.yaml", []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err = root.ReadFile("config.yaml")
	if err != nil || string(data) != "config" {
		t.Fatalf("root-level round trip = %q, %v", data, err)
	}
}

func TestOpenForTestRejectsSymlinkAndUnsafeMode(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(parent, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForTest(filepath.Join(parent, "link")); err == nil {
		t.Fatal("accepted symlink root")
	}
	unsafe := filepath.Join(parent, "unsafe")
	if err := os.Mkdir(unsafe, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenForTest(unsafe); err == nil {
		t.Fatal("accepted group-writable root")
	}
}

func TestOpenFileRejectsEscapesAndSymlinks(t *testing.T) {
	parent := t.TempDir()
	root, err := OpenForTest(filepath.Join(parent, "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := filepath.Join(parent, "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.OpenFile("../outside", unix.O_RDONLY, 0); err == nil {
		t.Fatal("accepted parent escape")
	}
	if err := os.Symlink(outside, filepath.Join(root.Path(), "state", "images", "link")); err != nil {
		t.Fatal(err)
	}
	_, err = root.OpenFile("state/images/link", unix.O_RDONLY, 0)
	if err == nil {
		t.Fatal("accepted symlink leaf")
	}
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.EXDEV) {
		t.Logf("openat2 rejected symlink with %v", err)
	}
}

func TestRemoveAndRenameRejectIntermediateSymlink(t *testing.T) {
	parent := t.TempDir()
	root, err := OpenForTest(filepath.Join(parent, "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(root.Path(), "logs")
	if err := os.Remove(logs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, logs); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove("logs/victim"); err == nil {
		t.Fatal("remove followed an intermediate symlink")
	}
	if err := os.WriteFile(filepath.Join(root.Path(), "state", "images", "source"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("state/images/source", "logs/victim"); err == nil {
		t.Fatal("rename followed an intermediate symlink")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "keep" {
		t.Fatalf("outside victim changed: %q, %v", data, err)
	}
}

func TestRemoveTreeDoesNotFollowSymlinks(t *testing.T) {
	parent := t.TempDir()
	root, err := OpenForTest(filepath.Join(parent, "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsureDir("workspaces/test", 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root.Path(), "workspaces", "test", "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Path(), "workspaces", "test", "local"), []byte("delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveTree("workspaces/test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root.Path(), "workspaces", "test")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tree remains: %v", err)
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "keep" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}
