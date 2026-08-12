//go:build linux

package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/runtime"
)

func TestCompileHonorsKindAndIsolatedCachePolicy(t *testing.T) {
	root, closeRoot := testRoot(t)
	defer closeRoot()
	permitted := t.TempDir()
	goRoot := directory(t, permitted, "go")
	rustRoot := directory(t, permitted, "rust")
	cargoHome := directory(t, permitted, "cargo")
	registry := directory(t, cargoHome, "registry")
	gitCache := directory(t, cargoHome, "git")
	selected := []Candidate{
		{Kind: "go", Root: goRoot, Compatible: true, Fingerprint: "go-fingerprint", CacheRoots: map[string]string{"modules": directory(t, permitted, "gomod")}},
		{Kind: "rust", Root: rustRoot, Compatible: true, Fingerprint: "rust-fingerprint", CacheRoots: map[string]string{"registry": registry, "git": gitCache}},
	}
	spec := config.HostToolchainsSpec{Enabled: true, Persistence: "workspace", Go: config.GoImportSpec{Enabled: false, Caches: "cow"}, Rust: config.RustImportSpec{Enabled: true, Caches: "isolated"}}
	plan, err := Compile(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", spec, selected, []string{permitted}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Overlays) != 0 {
		t.Fatalf("isolated policy produced overlays: %#v", plan.Overlays)
	}
	for _, mount := range plan.Mounts {
		if strings.Contains(mount.Target, "/go/") || mount.Source == registry || mount.Source == gitCache {
			t.Fatalf("unsafe or disabled mount: %#v", mount)
		}
	}
	if !containsEnvironment(plan.Environment, "CARGO_HOME=/cohotfs/toolchains/rust/state/cargo-home") || !containsEnvironment(plan.Environment, "RUSTC=/cohotfs/toolchains/rust/root/bin/rustc") {
		t.Fatalf("Rust managed environment = %#v", plan.Environment)
	}
	if containsEnvironmentPrefix(plan.Environment, "RUSTUP_HOME=") {
		t.Fatalf("rustup state exposed: %#v", plan.Environment)
	}
}

func TestCompileRequiresEveryRequestedCOWLower(t *testing.T) {
	root, closeRoot := testRoot(t)
	defer closeRoot()
	permitted := t.TempDir()
	candidate := Candidate{Kind: "go", Root: directory(t, permitted, "go"), Compatible: true, Fingerprint: "fingerprint", CacheRoots: map[string]string{"modules": filepath.Join(permitted, "missing")}}
	spec := config.HostToolchainsSpec{Enabled: true, Persistence: "workspace", RequireCOW: true, Go: config.GoImportSpec{Enabled: true, Caches: "cow"}, Rust: config.RustImportSpec{Caches: "isolated"}}
	if _, err := Compile(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", spec, []Candidate{candidate}, []string{permitted}, true); err == nil {
		t.Fatal("requireCow accepted a missing lower")
	}
	candidate.CacheRoots["modules"] = directory(t, permitted, "gomod")
	if _, err := Compile(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", spec, []Candidate{candidate}, []string{permitted}, false); err == nil {
		t.Fatal("requireCow accepted an unavailable overlay implementation")
	}
}

func TestCompileKeepsLowersOutOfWritableRuntimeMounts(t *testing.T) {
	root, closeRoot := testRoot(t)
	defer closeRoot()
	permitted := t.TempDir()
	lower := directory(t, permitted, "gomod")
	candidate := Candidate{Kind: "go", Root: directory(t, permitted, "go"), Compatible: true, Fingerprint: "fingerprint-a", CacheRoots: map[string]string{"modules": lower}}
	spec := config.HostToolchainsSpec{Enabled: true, Persistence: "workspace", Go: config.GoImportSpec{Enabled: true, Caches: "cow"}, Rust: config.RustImportSpec{Caches: "isolated"}}
	plan, err := Compile(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", spec, []Candidate{candidate}, []string{permitted}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Overlays) != 1 || plan.Overlays[0].Lower != lower || plan.Overlays[0].Fingerprint != "fingerprint-a" {
		t.Fatalf("overlay plan = %#v", plan.Overlays)
	}
	for _, mount := range plan.Mounts {
		if mount.Source == lower && !mount.ReadOnly {
			t.Fatalf("host lower is writable in runtime: %#v", mount)
		}
	}
	candidate.Fingerprint = "fingerprint-b"
	second, err := Compile(root, "aaaaaaaaaaaaaaaaaaaaaaaaaa", spec, []Candidate{candidate}, []string{permitted}, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Overlays[0].Upper == plan.Overlays[0].Upper {
		t.Fatal("changed fingerprint reused persistent upper")
	}
}

func TestActivateFallsBackBeforeRuntimeCreation(t *testing.T) {
	fallback := t.TempDir()
	plan := Plan{
		Mounts:   []runtime.Mount{{Source: "/not/activated", Target: "/cache", Type: "bind"}},
		Overlays: []OverlayMount{{Lower: "/path,with,delimiter", Upper: t.TempDir(), Work: t.TempDir(), Merged: t.TempDir(), Fallback: fallback, Target: "/cache"}},
	}
	resources, err := Activate(&plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 0 || len(plan.Overlays) != 0 || plan.Mounts[0].Source != fallback {
		t.Fatalf("fallback plan=%#v resources=%#v", plan, resources)
	}
	plan.Mounts[0].Source = "/not/activated"
	plan.Overlays = []OverlayMount{{Lower: "/path,with,delimiter", Upper: t.TempDir(), Work: t.TempDir(), Merged: t.TempDir(), Fallback: fallback, Target: "/cache"}}
	if _, err := Activate(&plan, true); err == nil {
		t.Fatal("requireCow accepted activation failure")
	}
}

func TestValidateManagedEnvironmentRejectsOverrides(t *testing.T) {
	for _, environment := range [][]string{{"GOMODCACHE=/host/cache"}, {"CARGO_HOME=/home/user/.cargo"}, {"GODEBUG=http2debug=1,installgoroot=all"}} {
		if err := ValidateManagedEnvironment(environment); err == nil {
			t.Fatalf("accepted environment %#v", environment)
		}
	}
}

func testRoot(t *testing.T) (*hostroot.Root, func()) {
	t.Helper()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	return root, func() { _ = root.Close() }
}

func TestProbeOverlayCleansItsMountAndDirectories(t *testing.T) {
	root, closeRoot := testRoot(t)
	defer closeRoot()
	if _, err := ProbeOverlay(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root.Path(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "overlay-probe-") {
			t.Fatalf("probe state remains: %s", entry.Name())
		}
	}
}

func directory(t *testing.T, base, name string) string {
	t.Helper()
	path := filepath.Join(base, name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsEnvironment(environment []string, expected string) bool {
	for _, item := range environment {
		if item == expected {
			return true
		}
	}
	return false
}

func containsEnvironmentPrefix(environment []string, prefix string) bool {
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}
