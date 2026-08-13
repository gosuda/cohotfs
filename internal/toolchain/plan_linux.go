//go:build linux

package toolchain

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
)

type OverlayMount struct {
	Lower       string `json:"lower"`
	Upper       string `json:"upper"`
	Work        string `json:"work"`
	Merged      string `json:"merged"`
	Fallback    string `json:"fallback"`
	Target      string `json:"target"`
	Fingerprint string `json:"fingerprint"`
	Executable  bool   `json:"executable,omitempty"`
}

type Plan struct {
	Mounts      []runtime.Mount `json:"mounts"`
	Overlays    []OverlayMount  `json:"overlays,omitempty"`
	Environment []string        `json:"environment"`
	Fallbacks   []string        `json:"fallbacks,omitempty"`
}

const managedToolchainPath = "/cohotfs/toolchains/go/state/bin:/cohotfs/toolchains/go/root/bin:/cohotfs/toolchains/rust/state/install/bin:/cohotfs/toolchains/rust/root/bin:/usr/local/bin:/usr/bin:/bin"

func Compile(root *hostroot.Root, workspaceID string, spec config.HostToolchainsSpec, selected []Candidate, permittedRoots []string, overlayAvailable bool) (Plan, error) {
	if !spec.Enabled {
		return Plan{}, nil
	}
	plan := Plan{}
	imported := false
	for _, candidate := range selected {
		enabled, cacheMode, err := importPolicy(spec, candidate.Kind)
		if err != nil {
			return Plan{}, err
		}
		if !enabled {
			continue
		}
		if !candidate.Compatible {
			plan.Fallbacks = append(plan.Fallbacks, candidate.Kind+": image toolchain used: "+candidate.Reason)
			continue
		}
		if !pathPermitted(candidate.Root, permittedRoots) {
			return Plan{}, fmt.Errorf("%s toolchain root %s is outside permittedRoots; run cohotfs onboard or grant the root in config.yaml", candidate.Kind, candidate.Root)
		}
		imported = true
		containerRoot := "/cohotfs/toolchains/" + candidate.Kind + "/root"
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: candidate.Root, Target: containerRoot, Type: "bind", ReadOnly: true, Propagation: "rprivate"})

		stateRel := filepath.Join(persistenceRoot(spec.Persistence), workspaceID, "toolchains", candidate.Kind, "state")
		if err := root.EnsureDir(stateRel, 0o700); err != nil {
			return Plan{}, err
		}
		statePath, _ := root.HostPath(stateRel)
		stateTarget := "/cohotfs/toolchains/" + candidate.Kind + "/state"
		plan.Mounts = append(plan.Mounts, runtime.Mount{Source: statePath, Target: stateTarget, Type: "bind", Propagation: "rprivate"})

		cacheNames := make([]string, 0, len(candidate.CacheRoots))
		for name := range candidate.CacheRoots {
			cacheNames = append(cacheNames, name)
		}
		sort.Strings(cacheNames)
		for _, name := range cacheNames {
			hostCache := candidate.CacheRoots[name]
			containerCache := cacheTarget(candidate.Kind, name)
			canOverlay := cacheMode == "cow" && overlayAvailable && directoryExists(hostCache) && pathPermitted(hostCache, permittedRoots)
			if cacheMode == "cow" && !canOverlay && spec.RequireCOW {
				return Plan{}, fmt.Errorf("%s %s cache cannot use COW and requireCow is true", candidate.Kind, name)
			}
			fallback, err := prepareIsolatedCache(root, spec.Persistence, workspaceID, candidate.Kind, name)
			if err != nil {
				return Plan{}, err
			}
			if canOverlay {
				baseRel := filepath.Join(persistenceRoot(spec.Persistence), workspaceID, "toolchains", candidate.Kind, candidate.Fingerprint, name)
				overlay, err := prepareOverlayPaths(root, baseRel, hostCache, containerCache, candidate.Fingerprint, fallback)
				if err != nil {
					return Plan{}, err
				}
				plan.Overlays = append(plan.Overlays, overlay)
				plan.Mounts = append(plan.Mounts, runtime.Mount{Source: overlay.Merged, Target: containerCache, Type: "bind", Propagation: "rprivate"})
				continue
			}
			plan.Mounts = append(plan.Mounts, runtime.Mount{Source: fallback, Target: containerCache, Type: "bind", Propagation: "rprivate"})
			reason := "configured isolated cache"
			if cacheMode == "cow" {
				reason = "COW unavailable; isolated cache"
			}
			plan.Fallbacks = append(plan.Fallbacks, candidate.Kind+" "+name+": "+reason)
		}
		if err := addManagedState(root, stateRel, candidate.Kind, containerRoot, &plan); err != nil {
			return Plan{}, err
		}
	}
	if imported {
		plan.Environment = append(plan.Environment, "COHOTFS_MANAGED_TOOLCHAINS=1", "PATH="+managedToolchainPath)
	}
	sort.SliceStable(plan.Mounts, func(i, j int) bool { return plan.Mounts[i].Target < plan.Mounts[j].Target })
	return plan, nil
}

func importPolicy(spec config.HostToolchainsSpec, kind string) (bool, string, error) {
	switch kind {
	case "go":
		if spec.Go.Caches != "cow" && spec.Go.Caches != "isolated" {
			return false, "", fmt.Errorf("Go caches must be cow or isolated")
		}
		return spec.Go.Enabled, spec.Go.Caches, nil
	case "rust":
		if spec.Rust.Caches != "cow" && spec.Rust.Caches != "isolated" {
			return false, "", fmt.Errorf("Rust caches must be cow or isolated")
		}
		return spec.Rust.Enabled, spec.Rust.Caches, nil
	default:
		return false, "", fmt.Errorf("unsupported toolchain kind %q", kind)
	}
}

func prepareOverlayPaths(root *hostroot.Root, baseRel, lower, target, fingerprint, fallback string) (OverlayMount, error) {
	upperRel := filepath.Join(baseRel, "upper")
	workRel := filepath.Join(baseRel, "work")
	mergedRel := filepath.Join(baseRel, "merged")
	for _, relative := range []string{upperRel, workRel, mergedRel} {
		if err := root.EnsureDir(relative, 0o700); err != nil {
			return OverlayMount{}, err
		}
	}
	upper, _ := root.HostPath(upperRel)
	work, _ := root.HostPath(workRel)
	merged, _ := root.HostPath(mergedRel)
	return OverlayMount{Lower: lower, Upper: upper, Work: work, Merged: merged, Fallback: fallback, Target: target, Fingerprint: fingerprint}, nil
}

func prepareIsolatedCache(root *hostroot.Root, persistence, workspaceID, kind, name string) (string, error) {
	relative := filepath.Join(persistenceRoot(persistence), workspaceID, "toolchains", kind, "isolated", name)
	if err := root.EnsureDir(relative, 0o700); err != nil {
		return "", err
	}
	return root.HostPath(relative)
}

func addManagedState(root *hostroot.Root, stateRel, kind, containerRoot string, plan *Plan) error {
	switch kind {
	case "go":
		for _, directory := range []string{"gopath", "bin", "tmp"} {
			if err := root.EnsureDir(filepath.Join(stateRel, directory), 0o700); err != nil {
				return err
			}
		}
		plan.Environment = append(plan.Environment,
			"GOROOT="+containerRoot,
			"GOTOOLCHAIN=local",
			"GOMODCACHE=/cohotfs/toolchains/go/cache/modules",
			"GOCACHE=/cohotfs/toolchains/go/cache/build",
			"GOPATH=/cohotfs/toolchains/go/state/gopath",
			"GOBIN=/cohotfs/toolchains/go/state/bin",
			"GOENV=/cohotfs/toolchains/go/state/goenv",
			"GOTMPDIR=/cohotfs/toolchains/go/state/tmp",
		)
	case "rust":
		for _, directory := range []string{"cargo-home", "target", "install", "tmp"} {
			if err := root.EnsureDir(filepath.Join(stateRel, directory), 0o700); err != nil {
				return err
			}
		}
		plan.Environment = append(plan.Environment,
			"RUSTC=/cohotfs/toolchains/rust/root/bin/rustc",
			"RUSTDOC=/cohotfs/toolchains/rust/root/bin/rustdoc",
			"CARGO_HOME=/cohotfs/toolchains/rust/state/cargo-home",
			"CARGO_TARGET_DIR=/cohotfs/toolchains/rust/state/target",
			"CARGO_INSTALL_ROOT=/cohotfs/toolchains/rust/state/install",
			"TMPDIR=/cohotfs/toolchains/rust/state/tmp",
		)
	}
	return nil
}

func cacheTarget(kind, name string) string {
	if kind == "rust" {
		return "/cohotfs/toolchains/rust/state/cargo-home/" + name
	}
	return "/cohotfs/toolchains/go/cache/" + name
}

func persistenceRoot(persistence string) string {
	if persistence == "session" {
		return "run"
	}
	return "workspaces"
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func pathPermitted(path string, permittedRoots []string) bool {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	for _, root := range permittedRoots {
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(canonicalRoot, canonical)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func ValidateManagedEnvironment(environment []string) error {
	managed := map[string]string{
		"GOROOT":             "/cohotfs/toolchains/go/root",
		"GOTOOLCHAIN":        "local",
		"GOMODCACHE":         "/cohotfs/toolchains/go/cache/modules",
		"GOCACHE":            "/cohotfs/toolchains/go/cache/build",
		"GOPATH":             "/cohotfs/toolchains/go/state/gopath",
		"GOBIN":              "/cohotfs/toolchains/go/state/bin",
		"GOENV":              "/cohotfs/toolchains/go/state/goenv",
		"GOTMPDIR":           "/cohotfs/toolchains/go/state/tmp",
		"RUSTC":              "/cohotfs/toolchains/rust/root/bin/rustc",
		"RUSTDOC":            "/cohotfs/toolchains/rust/root/bin/rustdoc",
		"CARGO_HOME":         "/cohotfs/toolchains/rust/state/cargo-home",
		"CARGO_TARGET_DIR":   "/cohotfs/toolchains/rust/state/target",
		"CARGO_INSTALL_ROOT": "/cohotfs/toolchains/rust/state/install",
	}
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if key == "GODEBUG" && strings.Contains(value, "installgoroot=all") {
			return fmt.Errorf("GODEBUG=installgoroot=all is incompatible with an immutable GOROOT")
		}
		if expected, managedKey := managed[key]; managedKey && value != expected {
			return fmt.Errorf("%s points outside the managed toolchain path", key)
		}
	}
	return nil
}

// SetupEnvironment returns only generated toolchain variables that setup
// commands may inherit. Without the managed marker, host process variables are
// ignored rather than copied into the container setup environment.
func SetupEnvironment(environment []string) ([]string, error) {
	managed := false
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if found && key == "COHOTFS_MANAGED_TOOLCHAINS" {
			if value != "1" {
				return nil, fmt.Errorf("COHOTFS_MANAGED_TOOLCHAINS must be 1")
			}
			managed = true
		}
	}
	if !managed {
		return nil, nil
	}
	result := make([]string, 0, 16)
	seen := make(map[string]bool)
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		if key == "TMPDIR" && value == "/home/agent/.tmp" {
			continue
		}
		expected, allowed := setupEnvironmentValue(key)
		if !allowed {
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate managed setup variable %s", key)
		}
		if value != expected {
			return nil, fmt.Errorf("%s points outside the managed toolchain path", key)
		}
		seen[key] = true
		result = append(result, item)
	}
	return result, nil
}

func setupEnvironmentValue(key string) (string, bool) {
	switch key {
	case "COHOTFS_MANAGED_TOOLCHAINS":
		return "1", true
	case "PATH":
		return managedToolchainPath, true
	case "GOROOT":
		return "/cohotfs/toolchains/go/root", true
	case "GOTOOLCHAIN":
		return "local", true
	case "GOMODCACHE":
		return "/cohotfs/toolchains/go/cache/modules", true
	case "GOCACHE":
		return "/cohotfs/toolchains/go/cache/build", true
	case "GOPATH":
		return "/cohotfs/toolchains/go/state/gopath", true
	case "GOBIN":
		return "/cohotfs/toolchains/go/state/bin", true
	case "GOENV":
		return "/cohotfs/toolchains/go/state/goenv", true
	case "GOTMPDIR":
		return "/cohotfs/toolchains/go/state/tmp", true
	case "RUSTC":
		return "/cohotfs/toolchains/rust/root/bin/rustc", true
	case "RUSTDOC":
		return "/cohotfs/toolchains/rust/root/bin/rustdoc", true
	case "CARGO_HOME":
		return "/cohotfs/toolchains/rust/state/cargo-home", true
	case "CARGO_TARGET_DIR":
		return "/cohotfs/toolchains/rust/state/target", true
	case "CARGO_INSTALL_ROOT":
		return "/cohotfs/toolchains/rust/state/install", true
	case "TMPDIR":
		return "/cohotfs/toolchains/rust/state/tmp", true
	default:
		return "", false
	}
}
