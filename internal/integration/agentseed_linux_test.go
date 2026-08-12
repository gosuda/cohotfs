//go:build linux

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
)

func TestStageAgentSeedsCopiesOnlyAllowlistedNonSecrets(t *testing.T) {
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	claude := filepath.Join(home, ".claude")
	omp := filepath.Join(home, ".config", "omp", "profiles", "default")
	for _, directory := range []string{codex, claude, omp} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixtures := map[string]string{
		filepath.Join(codex, "config.toml"):    "model = 'fixture'\n",
		filepath.Join(codex, "auth.json"):      "secret",
		filepath.Join(claude, "settings.json"): "{\"theme\":\"dark\"}\n",
		filepath.Join(home, ".claude.json"):    "secret",
		filepath.Join(omp, "config.yaml"):      "model: fixture\n",
		filepath.Join(omp, "agent.db"):         "secret",
	}
	for path, content := range fixtures {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	agents := config.AgentsSpec{OMP: config.AgentSpec{Enabled: true, Config: "seed"}, Codex: config.AgentSpec{Enabled: true, Config: "seed"}, Claude: config.AgentSpec{Enabled: true, Config: "seed"}}
	staged, err := StageAgentSeeds(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", agents, []string{"HOME=" + home})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 3 {
		t.Fatalf("staged = %#v", staged)
	}
	for _, seed := range staged {
		info, err := os.Stat(seed.Source)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("seed mode = %v err=%v", info.Mode(), err)
		}
		raw, err := os.ReadFile(seed.Source)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) == "secret" {
			t.Fatal("secret seed was staged")
		}
	}
}

func TestStageAgentSeedsRejectsSymlinkedAllowlistedFile(t *testing.T) {
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(outside, []byte("unsafe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(codex, "config.toml")); err != nil {
		t.Fatal(err)
	}
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	agents := config.AgentsSpec{Codex: config.AgentSpec{Enabled: true, Config: "seed"}}
	if _, err := StageAgentSeeds(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", agents, []string{"HOME=" + home}); err == nil {
		t.Fatal("accepted symlink seed")
	}
}
