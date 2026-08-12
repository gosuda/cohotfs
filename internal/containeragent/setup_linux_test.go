//go:build linux

package containeragent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBoundedSetupOutput(t *testing.T) {
	buffer := &boundedBuffer{limit: 4}
	count, err := buffer.Write([]byte("abcdefgh"))
	if err != nil || count != 8 || buffer.String() != "abcd" || !buffer.truncated {
		t.Fatalf("bounded write = %d %q truncated=%v err=%v", count, buffer.String(), buffer.truncated, err)
	}
}

func TestRunSetupExitAndManagedEnvironment(t *testing.T) {
	if err := os.MkdirAll("/workspace", 0o755); err != nil && !os.IsPermission(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat("/workspace"); err != nil {
		t.Skip("test cannot create /workspace")
	}
	script := filepath.Join(t.TempDir(), "setup.sh")
	content := "#!/bin/sh\nprintf '%s|%s|%s' \"$HOME\" \"$XDG_CONFIG_HOME\" \"${OPENAI_API_KEY-unset}\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("OPENAI_API_KEY", "fixture-secret"); err != nil {
		t.Fatal(err)
	}
	result, err := RunSetup(context.Background(), time.Second, []string{script})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Output), "fixture-secret") || string(result.Output) != "/home/agent|/home/agent/.config|unset" {
		t.Fatalf("managed environment output = %q", result.Output)
	}
}
