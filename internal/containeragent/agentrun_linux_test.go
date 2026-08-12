//go:build linux

package containeragent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRunAgentFetchesSecretOnceAndScrubsExistingDestination(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "secret.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var used atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if used.Swap(true) {
			http.Error(response, `{"error":{"message":"used"}}`, http.StatusGone)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"destination": "OPENAI_API_KEY", "value": []byte("fixture-secret")})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	script := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$OPENAI_API_KEY\" = fixture-secret ] && printf ok\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "wrong-value")
	var output lockedBuffer
	workspaceID := "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := RunAgent(context.Background(), socket, workspaceID, []string{"one-use"}, []string{script}, nil, &output, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "ok" {
		t.Fatalf("agent output = %q", output.String())
	}
	if _, err := fetchAgentSecret(context.Background(), socket, workspaceID, "one-use"); err == nil {
		t.Fatal("one-use agent secret was replayed")
	}
}

type lockedBuffer struct{ value []byte }

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.value = append(b.value, value...)
	return len(value), nil
}
func (b *lockedBuffer) String() string { return string(b.value) }
