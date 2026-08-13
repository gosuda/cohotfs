//go:build linux

package containeragent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallSeedsCopiesIntoPrivateHomeAndRefreshesOnRestart(t *testing.T) {
	sourceRoot := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "codex", "config.toml"), []byte("model='fixture'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := SeedManifest{SchemaVersion: 1, Seeds: []SeedEntry{{Source: "codex/config.toml", Destination: ".codex/config.toml", Mode: 0o600}}}
	raw, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallSeeds(manifestPath, sourceRoot, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(installed)
	if err != nil || string(data) != "model='fixture'\n" {
		t.Fatalf("installed = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "codex", "config.toml"), []byte("model='updated'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallSeeds(manifestPath, sourceRoot, home, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(installed)
	if err != nil || string(data) != "model='updated'\n" {
		t.Fatalf("refreshed = %q, %v", data, err)
	}
}

func TestInstallSeedsRejectsEscapesAndSymlinks(t *testing.T) {
	sourceRoot := t.TempDir()
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sourceRoot, "config.toml")); err != nil {
		t.Fatal(err)
	}
	manifest := SeedManifest{SchemaVersion: 1, Seeds: []SeedEntry{{Source: "config.toml", Destination: ".codex/config.toml", Mode: 0o600}}}
	raw, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallSeeds(manifestPath, sourceRoot, home, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("installed symlink seed")
	}
	manifest.Seeds[0].Source = "../outside"
	raw, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallSeeds(manifestPath, sourceRoot, home, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("installed escaping seed")
	}
}
