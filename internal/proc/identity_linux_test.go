//go:build linux

package proc

import (
	"os"
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

func TestSanitizedEnvironmentDropsSecrets(t *testing.T) {
	got := SanitizedEnvironment([]string{"PATH=/bin", "DISPLAY=:0", "OPENAI_API_KEY=fixture-secret", "SSH_AUTH_SOCK=/tmp/agent"})
	if len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "DISPLAY=:0" {
		t.Fatalf("sanitized environment = %v", got)
	}
}
