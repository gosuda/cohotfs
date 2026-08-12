package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceRoundTripAndStrictUnknownFields(t *testing.T) {
	want := BuiltinWorkspace("api-service", "example.invalid/workspace:dev")
	raw, err := Render(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeWorkspace(raw)
	if err != nil {
		t.Fatalf("decode rendered workspace:\n%s\n%v", raw, err)
	}
	if got.Metadata.Name != want.Metadata.Name || got.Spec.Setup.Timeout != 15*time.Minute || got.Spec.Resources.Memory != 4<<30 {
		t.Fatalf("unexpected round trip: %#v", got)
	}
	malicious := bytes.Replace(raw, []byte("metadata:\n"), []byte("metadata:\n  unexpected: true\n"), 1)
	if _, err := DecodeWorkspace(malicious); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestWorkspaceValidationFailsClosed(t *testing.T) {
	workspace := BuiltinWorkspace("safe", "example.invalid/workspace:dev")
	workspace.Spec.Image.Build = &BuildSpec{Context: ".", Containerfile: "Containerfile"}
	if err := workspace.Validate(); err == nil {
		t.Fatal("accepted both image ref and build")
	}
	workspace = BuiltinWorkspace("safe", "example.invalid/workspace:dev")
	workspace.Spec.Image.PullPolicy = "sometimes"
	if err := workspace.Validate(); err == nil {
		t.Fatal("accepted unknown image pull policy")
	}
	workspace = BuiltinWorkspace("safe", "example.invalid/workspace:dev")
	workspace.Spec.Resources.Enabled = true
	workspace.Spec.Resources.MemorySwap = workspace.Spec.Resources.Memory - 1
	if err := workspace.Validate(); err == nil {
		t.Fatal("accepted swap below memory")
	}
}

func TestByteSizeParsing(t *testing.T) {
	for input, want := range map[string]ByteSize{"4GiB": 4 << 30, "1024KiB": 1 << 20, "17": 17} {
		got, err := ParseByteSize(input)
		if err != nil || got != want {
			t.Fatalf("ParseByteSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	if _, err := ParseByteSize("-1GiB"); err == nil {
		t.Fatal("accepted negative size")
	}
}

func TestHostConfigCredentialDestinationAllowlist(t *testing.T) {
	host := BuiltinHostConfig()
	host.Credentials.AgentEnvironment["codex"]["UNSAFE_DESTINATION"] = "HOST_TOKEN"
	if err := host.Validate(); err == nil {
		t.Fatal("accepted arbitrary credential destination")
	}
}

func TestSparseHostConfigRetainsBuiltins(t *testing.T) {
	raw := []byte(`apiVersion: cohotfs.io/v1alpha1
kind: HostConfig
browser:
  linuxExecutable: /opt/chrome/chrome
`)
	host, err := DecodeHost(raw)
	if err != nil {
		t.Fatal(err)
	}
	if host.Runtime.Docker.Endpoint != "auto" {
		t.Fatalf("runtime defaults lost: %#v", host.Runtime)
	}
	if host.Toolchains.GoRoot != "auto" || host.Toolchains.RustToolchain != "auto" {
		t.Fatalf("toolchain defaults lost: %#v", host.Toolchains)
	}
	if host.Browser.LinuxExecutable != "/opt/chrome/chrome" {
		t.Fatalf("browser overlay lost: %#v", host.Browser)
	}
	for _, agent := range []string{"omp", "codex", "claude"} {
		if _, ok := host.Credentials.AgentEnvironment[agent]; !ok {
			t.Fatalf("missing built-in credential map for %s", agent)
		}
	}
}

func TestSparseHostConfigRejectsUnknownKey(t *testing.T) {
	raw := []byte(`apiVersion: cohotfs.io/v1alpha1
kind: HostConfig
runtime:
  prefered: docker
`)
	if _, err := DecodeHost(raw); err == nil {
		t.Fatal("accepted misspelled host key beside inherited preferred")
	}
}
