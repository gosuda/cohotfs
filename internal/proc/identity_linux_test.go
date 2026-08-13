//go:build linux

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCurrentIdentityRoundTrip(t *testing.T) {
	identity, err := CurrentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.PID != os.Getpid() || identity.UID != os.Getuid() || identity.StartTicks == 0 || identity.ExecutableDigest == "" {
		t.Fatalf("incomplete identity: %#v", identity)
	}
	if err := Matches(identity); err != nil {
		t.Fatal(err)
	}
	identity.StartTicks++
	if err := Matches(identity); err == nil {
		t.Fatal("accepted changed process start time")
	}
}

func TestReadIdentitySurvivesDeletedExecutablePath(t *testing.T) {
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "sleep")
	if err := os.WriteFile(executable, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	before, err := ReadIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	after, err := ReadIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("identity changed after executable unlink: before=%#v after=%#v", before, after)
	}
}

func TestSanitizedEnvironmentDropsSecrets(t *testing.T) {
	got := SanitizedEnvironment([]string{"PATH=/bin", "DISPLAY=:0", "OPENAI_API_KEY=fixture-secret", "SSH_AUTH_SOCK=/tmp/agent"})
	if len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "DISPLAY=:0" {
		t.Fatalf("sanitized environment = %v", got)
	}
}
