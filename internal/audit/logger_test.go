package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/hostroot"
)

func TestLoggerWritesBoundedTypedEvents(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	logger := New(root)
	logger.maxBytes = 180
	logger.now = func() time.Time { return time.Unix(10, 0) }
	for range 5 {
		if err := logger.Append(Event{Operation: "workspace.start", WorkspaceID: "workspace", ResourceType: "container", Result: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	active, err := os.ReadFile(filepath.Join(root.Path(), "logs", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(filepath.Join(root.Path(), "logs", "audit.1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(active, []byte("token")) || bytes.Contains(rotated, []byte("token")) {
		t.Fatal("audit schema contained secret-like payload")
	}
	for _, name := range []string{"audit.jsonl", "audit.1.jsonl"} {
		info, err := os.Stat(filepath.Join(root.Path(), "logs", name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 || info.Size() > logger.maxBytes {
			t.Fatalf("%s mode/size = %04o/%d", name, info.Mode().Perm(), info.Size())
		}
	}
}
