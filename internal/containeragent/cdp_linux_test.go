//go:build linux

package containeragent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteWebSocketURLsOnlyReplacesLoopbackDiscovery(t *testing.T) {
	input := []byte(`[{"webSocketDebuggerUrl":"ws://127.0.0.1:45123/devtools/page/1","nested":{"webSocketDebuggerUrl":"ws://localhost:45123/devtools/page/2"},"other":"ws://127.0.0.1:45123/keep"},{"webSocketDebuggerUrl":"ws://example.com/devtools/page/3"}]`)
	output := rewriteWebSocketURLs(input, "127.0.0.1:9222")
	var values []map[string]any
	if err := json.Unmarshal(output, &values); err != nil {
		t.Fatal(err)
	}
	if got := values[0]["webSocketDebuggerUrl"]; got != "ws://127.0.0.1:9222/devtools/page/1" {
		t.Fatalf("rewritten URL = %v", got)
	}
	if !strings.Contains(string(output), `"other":"ws://127.0.0.1:45123/keep"`) || !strings.Contains(string(output), `ws://example.com/devtools/page/3`) {
		t.Fatalf("unrelated URLs changed: %s", output)
	}
}
