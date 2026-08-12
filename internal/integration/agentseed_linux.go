//go:build linux

// Package integration implements explicit host-integration grants.
package integration

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"golang.org/x/sys/unix"
)

type SeedFile struct {
	Agent       string `json:"agent"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        uint32 `json:"mode"`
}

type seedCandidate struct {
	source      string
	destination string
}

func StageAgentSeeds(root *hostroot.Root, workspaceID string, agents config.AgentsSpec, environment []string) ([]SeedFile, error) {
	specs := []struct {
		name    string
		enabled bool
		mode    string
	}{
		{"omp", agents.OMP.Enabled, agents.OMP.Config},
		{"codex", agents.Codex.Enabled, agents.Codex.Config},
		{"claude", agents.Claude.Enabled, agents.Claude.Config},
	}
	var staged []SeedFile
	for _, agent := range specs {
		if !agent.enabled || agent.mode != "seed" {
			continue
		}
		for _, candidate := range seedCandidates(agent.name, environment) {
			seed, found, err := stageSeed(root, workspaceID, agent.name, candidate)
			if err != nil {
				return nil, err
			}
			if found {
				staged = append(staged, seed)
			}
		}
	}
	return staged, nil
}

func seedCandidates(agent string, environment []string) []seedCandidate {
	home := environmentValue(environment, "HOME")
	xdgConfig := environmentValueDefault(environment, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	switch agent {
	case "omp":
		root := environmentValue(environment, "PI_CONFIG_DIR")
		if root == "" {
			profile := environmentValue(environment, "OMP_PROFILE")
			if profile == "" {
				profile = "default"
			}
			root = filepath.Join(xdgConfig, "omp", "profiles", profile)
		}
		return []seedCandidate{{filepath.Join(root, "config.yaml"), ".config/omp/config.yaml"}, {filepath.Join(root, "settings.json"), ".config/omp/settings.json"}}
	case "codex":
		root := environmentValueDefault(environment, "CODEX_HOME", filepath.Join(home, ".codex"))
		return []seedCandidate{{filepath.Join(root, "config.toml"), ".codex/config.toml"}, {filepath.Join(root, "instructions.md"), ".codex/instructions.md"}}
	case "claude":
		root := environmentValueDefault(environment, "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
		return []seedCandidate{{filepath.Join(root, "settings.json"), ".claude/settings.json"}, {filepath.Join(root, "CLAUDE.md"), ".claude/CLAUDE.md"}}
	default:
		return nil
	}
}

func stageSeed(root *hostroot.Root, workspaceID, agent string, candidate seedCandidate) (SeedFile, bool, error) {
	name := strings.ToLower(filepath.Base(candidate.source))
	if secretSeedName(name) {
		return SeedFile{}, false, fmt.Errorf("refuse secret-bearing %s seed %s", agent, name)
	}
	inputFD, err := unix.Open(candidate.source, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return SeedFile{}, false, nil
	}
	if err != nil {
		return SeedFile{}, false, err
	}
	input := os.NewFile(uintptr(inputFD), candidate.source)
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return SeedFile{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return SeedFile{}, false, fmt.Errorf("%s seed must be a non-symlink regular file at most 1 MiB", agent)
	}
	raw, err := io.ReadAll(io.LimitReader(input, 1<<20+1))
	if err != nil {
		return SeedFile{}, false, err
	}
	if len(raw) > 1<<20 {
		return SeedFile{}, false, fmt.Errorf("%s seed exceeds 1 MiB", agent)
	}
	directory := filepath.Join("workspaces", workspaceID, "agent-seeds", agent)
	if err := root.EnsureDir(directory, 0o700); err != nil {
		return SeedFile{}, false, err
	}
	relative := filepath.Join("workspaces", workspaceID, "agent-seeds", agent, filepath.Base(candidate.destination))
	if err := root.AtomicWrite(relative, raw, 0o600); err != nil {
		return SeedFile{}, false, err
	}
	hostPath, _ := root.HostPath(relative)
	return SeedFile{Agent: agent, Source: hostPath, Destination: candidate.destination, Mode: 0o600}, true, nil
}

func secretSeedName(name string) bool {
	for _, forbidden := range []string{"auth.json", "agent.db", ".claude.json", "credentials", "token", "session", "history", "cookie", "keyring"} {
		if name == forbidden || strings.Contains(name, forbidden) {
			return true
		}
	}
	return false
}

func environmentValue(environment []string, key string) string {
	return environmentValueDefault(environment, key, "")
}

func environmentValueDefault(environment []string, key, fallback string) string {
	prefix := key + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return fallback
}
