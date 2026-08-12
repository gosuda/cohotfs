//go:build linux

package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDiscoverUsesToolSpecificVersionCommands(t *testing.T) {
	bin := t.TempDir()
	home := t.TempDir()
	goRoot := filepath.Join(t.TempDir(), "go")
	goCache := filepath.Join(home, ".cache", "go-build")
	goModules := filepath.Join(home, "go", "pkg", "mod")
	for _, directory := range []string{filepath.Join(goRoot, "bin"), goCache, goModules} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	goScript := "#!/bin/sh\ncase \"$1\" in\nversion) printf 'go version go1.26.5 linux/" + runtime.GOARCH + "\\n' ;;\nenv) printf '%s\\n' '" + goRoot + "' linux '" + runtime.GOARCH + "' '" + goModules + "' '" + goCache + "' ;;\n*) exit 19 ;;\nesac\n"
	writeExecutable(t, filepath.Join(goRoot, "bin", "go"), goScript)
	if err := os.Symlink(filepath.Join(goRoot, "bin", "go"), filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}

	rustRoot := filepath.Join(t.TempDir(), "rust")
	cargoHome := filepath.Join(home, ".cargo")
	for _, directory := range []string{filepath.Join(rustRoot, "bin"), filepath.Join(cargoHome, "registry"), filepath.Join(cargoHome, "git")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, executable := range []string{"rustc", "rustdoc", "cargo"} {
		writeExecutable(t, filepath.Join(rustRoot, "bin", executable), "#!/bin/sh\nexit 0\n")
	}
	rustProxy := filepath.Join(bin, "rustc")
	writeExecutable(t, rustProxy, "#!/bin/sh\ncase \"$1\" in\n--version) printf 'rustc 1.88.0 (fixture)\\n' ;;\n--print) [ \"$2\" = sysroot ] || exit 22; printf '%s\\n' '"+rustRoot+"' ;;\n-vV) printf 'rustc 1.88.0\\nhost: "+rustTriple()+"\\n' ;;\n*) exit 23 ;;\nesac\n")

	candidates, err := Discover(context.Background(), []string{"PATH=" + bin, "HOME=" + home, "CARGO_HOME=" + cargoHome})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	byKind := map[string]Candidate{}
	for _, candidate := range candidates {
		byKind[candidate.Kind] = candidate
	}
	if got := byKind["go"]; !got.Compatible || got.Version != "go version go1.26.5 linux/"+runtime.GOARCH || got.Root != goRoot || got.CacheRoots["modules"] != goModules {
		t.Fatalf("Go candidate = %#v", got)
	}
	if got := byKind["rust"]; !got.Compatible || got.Version != "rustc 1.88.0 (fixture)" || got.Root != rustRoot || got.CacheRoots["registry"] != filepath.Join(cargoHome, "registry") || got.CacheRoots["git"] != filepath.Join(cargoHome, "git") {
		t.Fatalf("Rust candidate = %#v", got)
	}
	if _, exposed := byKind["rust"].CacheRoots["cargo"]; exposed {
		t.Fatal("whole Cargo home exposed")
	}
	if _, exposed := byKind["rust"].CacheRoots["rustup"]; exposed {
		t.Fatal("rustup state exposed")
	}
}

func TestDiscoverExecutesRustProxyBeforeResolvingRealToolchain(t *testing.T) {
	bin := t.TempDir()
	rustRoot := filepath.Join(t.TempDir(), "toolchain")
	if err := os.MkdirAll(filepath.Join(rustRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{"rustc", "rustdoc", "cargo"} {
		writeExecutable(t, filepath.Join(rustRoot, "bin", executable), "#!/bin/sh\nexit 0\n")
	}
	proxy := filepath.Join(bin, "rustup")
	writeExecutable(t, proxy, "#!/bin/sh\n[ \"$(basename \"$0\")\" = rustc ] || exit 31\ncase \"$1\" in\n--version) echo 'rustc proxy fixture' ;;\n--print) echo '"+rustRoot+"' ;;\n-vV) printf 'host: "+rustTriple()+"\\n' ;;\n*) exit 32 ;;\nesac\n")
	if err := os.Symlink(proxy, filepath.Join(bin, "rustc")); err != nil {
		t.Fatal(err)
	}
	candidates, err := Discover(context.Background(), []string{"PATH=" + bin + ":/usr/bin", "HOME=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Kind == "rust" {
			if !candidate.Compatible || candidate.Path != filepath.Join(rustRoot, "bin", "rustc") || candidate.Version != "rustc proxy fixture" {
				t.Fatalf("Rust proxy candidate = %#v", candidate)
			}
			return
		}
	}
	t.Fatal("Rust candidate not discovered")
}

func rustTriple() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64-unknown-linux-gnu"
	}
	return "x86_64-unknown-linux-gnu"
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
