//go:build linux

// Package ompimport compiles explicit host OMP imports into workspace-owned
// writable snapshots.
package ompimport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
	"golang.org/x/sys/unix"
)

const (
	containerBinaryDir = "/cohotfs/agents/omp/bin"
	containerBinary    = containerBinaryDir + "/omp"
	containerAgent     = "/home/agent/.omp/agent"
	containerNative    = "/home/agent/.omp/natives"
)

type Sources struct {
	Binary  string `json:"binary"`
	Natives string `json:"natives,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

type Plan struct {
	Mounts      []runtime.Mount `json:"mounts,omitempty"`
	Environment []string        `json:"environment,omitempty"`
}

func Discover(environment []string) (Sources, error) {
	binary, err := exec.LookPath("omp")
	if err != nil {
		return Sources{}, fmt.Errorf("OMP executable is unavailable: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return Sources{}, err
	}
	if err := requireRegular(binary, true); err != nil {
		return Sources{}, err
	}
	home := environmentValue(environment, "HOME")
	if home == "" {
		return Sources{}, fmt.Errorf("HOME is unavailable for OMP discovery")
	}
	configRoot := environmentValue(environment, "PI_CONFIG_DIR")
	if configRoot == "" {
		configRoot = filepath.Join(home, ".omp")
	}
	profile := environmentValue(environment, "OMP_PROFILE")
	agent := ""
	if profile != "" && profile != "default" {
		if profile == "." || profile == ".." || strings.ContainsAny(profile, `/\`) {
			return Sources{}, fmt.Errorf("OMP profile name is unsafe")
		}
		agent = filepath.Join(configRoot, "profiles", profile, "agent")
	} else {
		agent = environmentValue(environment, "PI_CODING_AGENT_DIR")
		if agent == "" {
			agent = filepath.Join(configRoot, "agent")
		}
	}
	agent, err = canonicalOptionalDirectory(agent)
	if err != nil {
		return Sources{}, err
	}
	natives, err := canonicalOptionalDirectory(filepath.Join(configRoot, "natives"))
	if err != nil {
		return Sources{}, err
	}
	return Sources{Binary: binary, Natives: natives, Agent: agent}, nil
}

func Compile(root *hostroot.Root, workspaceID string, spec config.OMPAgentSpec, sources Sources) (Plan, error) {
	if !spec.Enabled || !spec.Import.Enabled {
		return Plan{}, nil
	}
	if sources.Binary == "" {
		return Plan{}, fmt.Errorf("OMP binary source is required")
	}
	if err := root.EnsureDir(filepath.Join("workspaces", workspaceID, "home", ".omp"), 0o700); err != nil {
		return Plan{}, err
	}
	plan := Plan{Environment: []string{
		"PI_CODING_AGENT_DIR=" + containerAgent,
		"PATH=" + containerBinaryDir + ":/usr/local/bin:/usr/bin:/bin",
	}}
	if spec.Import.Binary {
		snapshot, err := prepareFileSnapshot(root, workspaceID, "binary", sources.Binary, filepath.Base(containerBinary), true, spec.Import.RequireCOW)
		if err != nil {
			return Plan{}, err
		}
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: snapshot, Target: containerBinaryDir, Type: "bind", Propagation: "rprivate"})
	}
	if spec.Import.Natives && sources.Natives != "" {
		snapshot, err := prepareDirectorySnapshot(root, workspaceID, "natives", sources.Natives, spec.Import.RequireCOW)
		if err != nil {
			return Plan{}, err
		}
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: snapshot, Target: containerNative, Type: "bind", Propagation: "rprivate"})
	}
	if spec.Import.OAuthDB {
		if sources.Agent == "" {
			return Plan{}, fmt.Errorf("OMP agent directory source is unavailable")
		}
		snapshot, err := prepareDirectorySnapshot(root, workspaceID, "agent", sources.Agent, spec.Import.RequireCOW)
		if err != nil {
			return Plan{}, err
		}
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: snapshot, Target: containerAgent, Type: "bind", Propagation: "rprivate"})
	} else if (spec.Import.Models || spec.Import.Config) && sources.Agent != "" {
		selected, err := selectedAgentFiles(spec.Import, sources.Agent)
		if err != nil {
			return Plan{}, err
		}
		if len(selected) != 0 {
			snapshot, err := prepareSelectedSnapshot(root, workspaceID, "agent", sources.Agent, selected, spec.Import.RequireCOW)
			if err != nil {
				return Plan{}, err
			}
			plan.Mounts = append(plan.Mounts, runtime.Mount{Source: snapshot, Target: containerAgent, Type: "bind", Propagation: "rprivate"})
		}
	}
	return plan, nil
}

func selectedAgentFiles(spec config.OMPImportSpec, root string) ([]string, error) {
	var candidates []string
	if spec.Config {
		candidates = append(candidates, "config.yml", "config.yaml")
	}
	if spec.Models {
		candidates = append(candidates, "models.yml", "models.yaml")
	}
	selected := make([]string, 0, len(candidates))
	for _, name := range candidates {
		path := filepath.Join(root, name)
		err := requireRegular(path, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("validate OMP import %s: %w", name, err)
		}
		selected = append(selected, name)
	}
	return selected, nil
}

func prepareFileSnapshot(root *hostroot.Root, workspaceID, name, source, destination string, executable, requireCOW bool) (string, error) {
	if err := requireRegular(source, executable); err != nil {
		return "", err
	}
	fingerprint, err := selectedFingerprint(filepath.Dir(source), []string{filepath.Base(source)})
	if err != nil {
		return "", err
	}
	return prepareSnapshot(root, workspaceID, name, fingerprint, func(staging string) error {
		return cloneRegularFile(source, filepath.Join(staging, destination), executable, requireCOW)
	})
}

func prepareSelectedSnapshot(root *hostroot.Root, workspaceID, name, source string, selected []string, requireCOW bool) (string, error) {
	fingerprint, err := selectedFingerprint(source, selected)
	if err != nil {
		return "", err
	}
	return prepareSnapshot(root, workspaceID, name, fingerprint, func(staging string) error {
		for _, relative := range selected {
			if err := cloneRegularFile(filepath.Join(source, relative), filepath.Join(staging, relative), false, requireCOW); err != nil {
				return err
			}
		}
		return nil
	})
}

func prepareDirectorySnapshot(root *hostroot.Root, workspaceID, name, source string, requireCOW bool) (string, error) {
	fingerprint, err := directoryFingerprint(source)
	if err != nil {
		return "", err
	}
	return prepareSnapshot(root, workspaceID, name, fingerprint, func(staging string) error {
		return cloneDirectory(source, staging, requireCOW)
	})
}

func prepareSnapshot(root *hostroot.Root, workspaceID, name, fingerprint string, populate func(string) error) (string, error) {
	parentRelative := filepath.Join("workspaces", workspaceID, "agents", "omp", name)
	if err := root.EnsureDir(parentRelative, 0o700); err != nil {
		return "", err
	}
	parent, err := root.HostPath(parentRelative)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(parent, fingerprint)
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("unsafe existing OMP snapshot %s", destination)
		}
		return destination, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", err
	}
	if err := populate(staging); err != nil {
		return "", err
	}
	if err := os.Rename(staging, destination); err != nil {
		if info, inspectErr := os.Lstat(destination); inspectErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return destination, nil
		}
		return "", err
	}
	return destination, nil
}

func cloneDirectory(source, destination string, requireCOW bool) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Mkdir(target, 0o700)
		case info.Mode().IsRegular():
			return cloneRegularFile(path, target, info.Mode().Perm()&0o111 != 0, requireCOW)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(relative), link))
			if filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return fmt.Errorf("OMP native symlink escapes its source: %s", relative)
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("unsupported OMP native file type: %s", relative)
		}
	})
}

func cloneRegularFile(source, destination string, executable, requireCOW bool) error {
	return writeRegularFile(source, destination, executable, true, requireCOW)
}

func writeRegularFile(source, destination string, executable, tryReflink, requireCOW bool) error {
	sourceFD, err := unix.Open(source, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	sourceFile := os.NewFile(uintptr(sourceFD), source)
	defer sourceFile.Close()
	var sourceStat unix.Stat_t
	if err := unix.Fstat(sourceFD, &sourceStat); err != nil {
		return err
	}
	if sourceStat.Mode&unix.S_IFMT != unix.S_IFREG || executable && sourceStat.Mode&0o111 == 0 {
		return fmt.Errorf("unsafe OMP file %s", source)
	}
	mode := uint32(sourceStat.Mode) & 0o777
	mode &^= 0o022
	destinationFD, err := unix.Open(destination, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	destinationFile := os.NewFile(uintptr(destinationFD), destination)
	defer destinationFile.Close()
	cloned := false
	if tryReflink {
		cloneErr := unix.IoctlFileClone(destinationFD, sourceFD)
		if cloneErr == nil {
			cloned = true
		} else if requireCOW {
			return fmt.Errorf("reflink OMP file %s: %w", source, cloneErr)
		}
	}
	if !cloned {
		if err := unix.Ftruncate(destinationFD, 0); err != nil {
			return err
		}
		if _, err := sourceFile.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(destinationFile, sourceFile); err != nil {
			return err
		}
	}
	if err := unix.Fchmod(destinationFD, mode); err != nil {
		return err
	}
	return destinationFile.Sync()
}

func directoryFingerprint(path string) (string, error) {
	hash := sha256.New()
	if _, err := fmt.Fprintf(hash, "%s\x00", path); err != nil {
		return "", err
	}
	if err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		return fingerprintEntry(hash, current, relative, entry)
	}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)[:16]), nil
}

func selectedFingerprint(root string, names []string) (string, error) {
	hash := sha256.New()
	if _, err := fmt.Fprintf(hash, "%s\x00", root); err != nil {
		return "", err
	}
	names = append([]string(nil), names...)
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(root, name)
		entry, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\x00", name, entry.Mode(), entry.Size(), entry.ModTime().UnixNano()); err != nil {
			return "", err
		}
		if err := fingerprintRegularFile(hash, path); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)[:16]), nil
}

func fingerprintEntry(hash interface{ Write([]byte) (int, error) }, path, relative string, entry os.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\x00", relative, info.Mode(), info.Size(), info.ModTime().UnixNano()); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(hash, "%s\x00", target)
		return err
	}
	if info.Mode().IsRegular() {
		return fingerprintRegularFile(hash, path)
	}
	return nil
}

func fingerprintRegularFile(hash io.Writer, path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("unsafe OMP file %s", path)
	}
	_, err = io.Copy(hash, file)
	return err
}

func requireRegular(path string, executable bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || executable && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("unsafe OMP file %s", path)
	}
	return nil
}

func canonicalOptionalDirectory(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("OMP path %s is not a directory", path)
	}
	return canonical, nil
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}
