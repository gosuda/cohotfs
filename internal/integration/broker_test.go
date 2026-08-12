package integration

import (
	"bytes"
	"context"
	"testing"
	"time"
)

type fixtureCredentialProvider struct {
	input []byte
}

func (p *fixtureCredentialProvider) Fill(_ context.Context, input []byte) ([]byte, error) {
	p.input = append([]byte(nil), input...)
	return []byte("username=fixture\npassword=secret\n\n"), nil
}

func TestGitCredentialExactContextAndGetOnly(t *testing.T) {
	provider := &fixtureCredentialProvider{}
	input := []byte("protocol=https\nhost=example.com:443\npath=org/repo\n\n")
	output, err := FillCredential(context.Background(), "get", input, []string{"https://example.com:443/org/repo"}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(provider.input, input) || !bytes.Contains(output, []byte("password=secret")) {
		t.Fatalf("provider input=%q output=%q", provider.input, output)
	}
	for _, test := range []struct{ operation, context string }{{"store", "https://example.com:443/org/repo"}, {"get", "https://example.com:443/org"}, {"get", "https://example.com/org/repo"}, {"get", "http://example.com:443/org/repo"}} {
		if _, err := FillCredential(context.Background(), test.operation, input, []string{test.context}, provider); err == nil {
			t.Fatalf("accepted operation=%s context=%s", test.operation, test.context)
		}
	}
}

func TestSecretLeaseIsOneUseBoundAndExpires(t *testing.T) {
	store := NewSecretStore()
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	id, expires, err := store.Issue("workspace", "codex", "OPENAI_API_KEY", []byte("fixture-secret"), 60*time.Second)
	if err != nil || !expires.Equal(now.Add(60*time.Second)) {
		t.Fatalf("issue = %q %v %v", id, expires, err)
	}
	if _, _, err := store.Fetch(id, "other"); err == nil {
		t.Fatal("secret fetched by another workspace")
	}
	destination, value, err := store.Fetch(id, "workspace")
	if err != nil || destination != "OPENAI_API_KEY" || string(value) != "fixture-secret" {
		t.Fatalf("fetch = %q %q %v", destination, value, err)
	}
	if _, _, err := store.Fetch(id, "workspace"); err == nil {
		t.Fatal("secret lease reused")
	}
	expiring, _, err := store.Issue("workspace", "claude", "ANTHROPIC_API_KEY", []byte("fixture"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, _, err := store.Fetch(expiring, "workspace"); err == nil {
		t.Fatal("expired secret fetched")
	}
	if _, _, err := store.Issue("workspace", "codex", "ARBITRARY", []byte("fixture"), time.Second); err == nil {
		t.Fatal("arbitrary destination accepted")
	}
}
