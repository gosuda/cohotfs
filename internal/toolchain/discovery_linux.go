//go:build linux

// Package toolchain discovers and plans immutable host toolchain imports.
package toolchain

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/gosuda/cohotfs/internal/config"
)

type Candidate struct {
	Kind         string            `json:"kind"`
	Path         string            `json:"path"`
	Root         string            `json:"root"`
	Version      string            `json:"version"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
	ABI          string            `json:"abi"`
	Executables  map[string]string `json:"executables"`
	CacheRoots   map[string]string `json:"cacheRoots"`
	Fingerprint  string            `json:"fingerprint"`
	Compatible   bool              `json:"compatible"`
	Reason       string            `json:"reason,omitempty"`
}

func Discover(ctx context.Context, environment []string) ([]Candidate, error) {
	paths := filepath.SplitList(environmentValue(environment, "PATH"))
	candidates := make([]Candidate, 0, 2)
	seen := make(map[string]bool)
	for _, descriptor := range []struct{ kind, executable string }{{"go", "go"}, {"rust", "rustc"}} {
		for _, directory := range paths {
			invocation := filepath.Join(directory, descriptor.executable)
			canonical, err := filepath.EvalSymlinks(invocation)
			if err != nil || seen[descriptor.kind+canonical] {
				continue
			}
			info, err := os.Stat(canonical)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}
			seen[descriptor.kind+canonical] = true
			var candidate Candidate
			if descriptor.kind == "go" {
				candidate = inspectGo(ctx, invocation, environment)
			} else {
				candidate = inspectRust(ctx, invocation, environment)
			}
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Kind != candidates[j].Kind {
			return candidates[i].Kind < candidates[j].Kind
		}
		return candidates[i].Path < candidates[j].Path
	})
	return candidates, nil
}

func inspectGo(ctx context.Context, invocation string, environment []string) Candidate {
	candidate := Candidate{Kind: "go", OS: "linux", ABI: hostABI(), Executables: map[string]string{}, CacheRoots: map[string]string{}, Compatible: true}
	version, err := commandOutput(ctx, environment, invocation, "version")
	if err != nil {
		return incompatible(candidate, err)
	}
	values, err := commandLines(ctx, environment, invocation, "env", "GOROOT", "GOOS", "GOARCH", "GOMODCACHE", "GOCACHE")
	if err != nil || len(values) != 5 {
		if err == nil {
			err = fmt.Errorf("go env returned %d fields", len(values))
		}
		return incompatible(candidate, err)
	}
	root, err := canonicalDirectory(values[0])
	if err != nil {
		return incompatible(candidate, fmt.Errorf("resolve GOROOT: %w", err))
	}
	realGo, err := canonicalExecutable(filepath.Join(root, "bin", "go"))
	if err != nil {
		return incompatible(candidate, fmt.Errorf("resolve GOROOT go: %w", err))
	}
	candidate.Path, candidate.Root, candidate.Version = realGo, root, version
	candidate.OS, candidate.Architecture = values[1], normalizeArchitecture(values[2])
	candidate.Executables["go"] = realGo
	candidate.CacheRoots["modules"] = values[3]
	candidate.CacheRoots["build"] = values[4]
	validateNative(&candidate)
	candidate.Fingerprint = fingerprint(candidate)
	return candidate
}

func inspectRust(ctx context.Context, invocation string, environment []string) Candidate {
	candidate := Candidate{Kind: "rust", OS: "linux", Executables: map[string]string{}, CacheRoots: map[string]string{}, Compatible: true}
	version, err := commandOutput(ctx, environment, invocation, "--version")
	if err != nil {
		return incompatible(candidate, err)
	}
	sysroot, err := commandOutput(ctx, environment, invocation, "--print", "sysroot")
	if err != nil {
		return incompatible(candidate, err)
	}
	verbose, err := commandOutput(ctx, environment, invocation, "-vV")
	if err != nil {
		return incompatible(candidate, err)
	}
	root, err := canonicalDirectory(sysroot)
	if err != nil {
		return incompatible(candidate, fmt.Errorf("resolve Rust sysroot: %w", err))
	}
	realRustc, err := canonicalExecutable(filepath.Join(root, "bin", "rustc"))
	if err != nil {
		return incompatible(candidate, fmt.Errorf("resolve real rustc: %w", err))
	}
	realCargo, err := canonicalExecutable(filepath.Join(root, "bin", "cargo"))
	if err != nil {
		return incompatible(candidate, fmt.Errorf("resolve real cargo: %w", err))
	}
	hostTriple := fieldValue(verbose, "host")
	candidate.Path, candidate.Root, candidate.Version = realRustc, root, version
	candidate.OS, candidate.Architecture, candidate.ABI = triplePlatform(hostTriple)
	candidate.Executables["rustc"], candidate.Executables["rustdoc"], candidate.Executables["cargo"] = realRustc, filepath.Join(root, "bin", "rustdoc"), realCargo
	cargoHome := environmentValueDefault(environment, "CARGO_HOME", filepath.Join(environmentValue(environment, "HOME"), ".cargo"))
	for _, name := range []string{"registry", "git"} {
		path := filepath.Join(cargoHome, name)
		if nativeLinuxPath(path) {
			candidate.CacheRoots[name] = path
		}
	}
	validateNative(&candidate)
	candidate.Fingerprint = fingerprint(candidate)
	return candidate
}

func Select(candidates []Candidate, kind, configured string) (Candidate, error) {
	selection := configured
	if configured != "" && configured != "auto" {
		if canonical, err := filepath.EvalSymlinks(configured); err == nil {
			selection = canonical
		}
	}
	matches := make([]Candidate, 0)
	for _, candidate := range candidates {
		if candidate.Kind != kind || !candidate.Compatible {
			continue
		}
		if selection == "auto" || selection == "" || candidate.Path == selection || candidate.Root == selection || filepath.Dir(candidate.Path) == selection {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return Candidate{}, fmt.Errorf("no compatible %s toolchain found", kind)
	}
	if len(matches) > 1 && (selection == "auto" || selection == "") {
		return Candidate{}, fmt.Errorf("multiple %s toolchains found; select one in config.yaml", kind)
	}
	return matches[0], nil
}

func validateNative(candidate *Candidate) {
	if candidate.OS != "linux" || candidate.Architecture != goruntime.GOARCH || !nativeLinuxPath(candidate.Root) {
		candidate.Compatible = false
		candidate.Reason = fmt.Sprintf("host toolchain is %s/%s at %s; need linux/%s on a native Linux path", candidate.OS, candidate.Architecture, candidate.Root, goruntime.GOARCH)
	}
}

func incompatible(candidate Candidate, err error) Candidate {
	candidate.Compatible, candidate.Reason = false, err.Error()
	candidate.Fingerprint = fingerprint(candidate)
	return candidate
}

func ResolveSelections(candidates []Candidate, spec config.HostToolchainsSpec, host config.HostToolchainsConfig) ([]Candidate, error) {
	requested := []struct {
		kind       string
		enabled    bool
		workspace  string
		configured string
	}{
		{kind: "go", enabled: spec.Go.Enabled, workspace: spec.Go.Root, configured: host.GoRoot},
		{kind: "rust", enabled: spec.Rust.Enabled, workspace: spec.Rust.Toolchain, configured: host.RustToolchain},
	}
	selected := make([]Candidate, 0, 2)
	for _, request := range requested {
		if !request.enabled {
			continue
		}
		configured := request.workspace
		if configured == "" || configured == "auto" {
			configured = request.configured
		}
		if configured == "" {
			configured = "auto"
		}
		if candidate, err := Select(candidates, request.kind, configured); err == nil {
			selected = append(selected, candidate)
			continue
		}
		matches := matchingCandidates(candidates, request.kind, configured)
		if len(matches) > 1 {
			return nil, fmt.Errorf("multiple %s toolchains found; select one in config.yaml", request.kind)
		}
		if len(matches) == 1 {
			selected = append(selected, matches[0])
			continue
		}
		selected = append(selected, Candidate{Kind: request.kind, Compatible: false, Reason: "no matching host toolchain discovered"})
	}
	return selected, nil
}

func matchingCandidates(candidates []Candidate, kind, configured string) []Candidate {
	selection := configured
	if configured != "" && configured != "auto" {
		if canonical, err := filepath.EvalSymlinks(configured); err == nil {
			selection = canonical
		}
	}
	var matches []Candidate
	for _, candidate := range candidates {
		if candidate.Kind != kind {
			continue
		}
		if selection == "" || selection == "auto" || candidate.Path == selection || candidate.Root == selection || filepath.Dir(candidate.Path) == selection {
			matches = append(matches, candidate)
		}
	}
	return matches
}
func canonicalDirectory(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return canonical, nil
}

func canonicalExecutable(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("not an executable regular file")
	}
	return canonical, nil
}

func commandOutput(ctx context.Context, environment []string, executable string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = environment
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func commandLines(ctx context.Context, environment []string, executable string, arguments ...string) ([]string, error) {
	output, err := commandOutput(ctx, environment, executable, arguments...)
	if err != nil {
		return nil, err
	}
	return strings.Split(output, "\n"), nil
}

func fieldValue(output, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func triplePlatform(triple string) (string, string, string) {
	parts := strings.Split(triple, "-")
	architecture := ""
	if len(parts) != 0 {
		architecture = normalizeArchitecture(parts[0])
	}
	operatingSystem := ""
	if strings.Contains(triple, "linux") {
		operatingSystem = "linux"
	}
	return operatingSystem, architecture, triple
}

func normalizeArchitecture(value string) string {
	switch value {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return value
	}
}

func nativeLinuxPath(path string) bool {
	clean := filepath.Clean(path)
	return filepath.IsAbs(clean) && clean != "/mnt/c" && !strings.HasPrefix(clean, "/mnt/c/")
}

func fingerprint(candidate Candidate) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%s\x00%s\x00", candidate.Root, candidate.Version, candidate.OS, candidate.Architecture, candidate.ABI)
	keys := make([]string, 0, len(candidate.CacheRoots)+len(candidate.Executables))
	for key := range candidate.CacheRoots {
		keys = append(keys, "cache:"+key)
	}
	for key := range candidate.Executables {
		keys = append(keys, "executable:"+key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kind, name, _ := strings.Cut(key, ":")
		path := candidate.CacheRoots[name]
		if kind == "executable" {
			path = candidate.Executables[name]
		}
		fmt.Fprintf(hash, "%s=%s:%s\x00", key, path, filesystemIdentity(path))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func filesystemIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

func hostABI() string {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "linux-unknown"
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), "="); ok {
			values[key] = strings.Trim(value, "\"")
		}
	}
	return values["ID"] + ":" + values["VERSION_ID"]
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
