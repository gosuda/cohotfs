package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeDockerContext(t *testing.T, configDirectory, name, endpoint string) {
	t.Helper()
	digest := sha256.Sum256([]byte(name))
	directory := filepath.Join(configDirectory, "contexts", "meta", hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"Endpoints":{"docker":{"Host":"` + endpoint + `"}}}`)
	if err := os.WriteFile(filepath.Join(directory, "meta.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDockerEndpointPrecedenceAndLocalPolicy(t *testing.T) {
	configDirectory := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDirectory)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	if err := os.WriteFile(filepath.Join(configDirectory, "config.json"), []byte(`{"currentContext":"rootless"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDockerContext(t, configDirectory, "rootless", "unix:///run/user/1000/docker.sock")

	endpoint, err := resolveDockerEndpoint("auto")
	if err != nil || endpoint != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("current context endpoint = %q, %v", endpoint, err)
	}

	t.Setenv("DOCKER_HOST", "unix:///tmp/docker.sock")
	endpoint, err = resolveDockerEndpoint("auto")
	if err != nil || endpoint != "unix:///tmp/docker.sock" {
		t.Fatalf("DOCKER_HOST endpoint = %q, %v", endpoint, err)
	}

	writeDockerContext(t, configDirectory, "remote", "tcp://docker.example:2376")
	t.Setenv("DOCKER_CONTEXT", "remote")
	if _, err := resolveDockerEndpoint("auto"); err == nil {
		t.Fatal("accepted a remote Docker context")
	}
	if _, err := resolveDockerEndpoint("ssh://docker.example"); err == nil {
		t.Fatal("accepted an explicitly configured SSH Docker endpoint")
	}
}
