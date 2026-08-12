package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

const defaultDockerEndpoint = "unix:///var/run/docker.sock"

type dockerCLIConfig struct {
	CurrentContext string `json:"currentContext"`
}

type dockerContextMetadata struct {
	Endpoints map[string]struct {
		Host string `json:"Host"`
	} `json:"Endpoints"`
}

func resolveDockerEndpoint(configured string) (string, error) {
	if configured != "" && configured != "auto" {
		return validateLocalDockerEndpoint(configured)
	}
	if contextName := os.Getenv("DOCKER_CONTEXT"); contextName != "" {
		if contextName == "default" {
			return defaultDockerEndpoint, nil
		}
		return resolveDockerContext(contextName)
	}
	if endpoint := os.Getenv("DOCKER_HOST"); endpoint != "" {
		return validateLocalDockerEndpoint(endpoint)
	}
	configDirectory, err := dockerConfigDirectory()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(configDirectory, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return defaultDockerEndpoint, nil
	}
	if err != nil {
		return "", fmt.Errorf("read Docker CLI config: %w", err)
	}
	var cliConfig dockerCLIConfig
	if err := json.Unmarshal(raw, &cliConfig); err != nil {
		return "", fmt.Errorf("decode Docker CLI config: %w", err)
	}
	if cliConfig.CurrentContext == "" || cliConfig.CurrentContext == "default" {
		return defaultDockerEndpoint, nil
	}
	return resolveDockerContextFrom(configDirectory, cliConfig.CurrentContext)
}

func resolveDockerContext(name string) (string, error) {
	configDirectory, err := dockerConfigDirectory()
	if err != nil {
		return "", err
	}
	return resolveDockerContextFrom(configDirectory, name)
}

func resolveDockerContextFrom(configDirectory, name string) (string, error) {
	digest := sha256.Sum256([]byte(name))
	metadataPath := filepath.Join(configDirectory, "contexts", "meta", hex.EncodeToString(digest[:]), "meta.json")
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", fmt.Errorf("read Docker context %q: %w", name, err)
	}
	var metadata dockerContextMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", fmt.Errorf("decode Docker context %q: %w", name, err)
	}
	endpoint, ok := metadata.Endpoints["docker"]
	if !ok || endpoint.Host == "" {
		return "", fmt.Errorf("Docker context %q has no Docker endpoint", name)
	}
	resolved, err := validateLocalDockerEndpoint(endpoint.Host)
	if err != nil {
		return "", fmt.Errorf("Docker context %q: %w", name, err)
	}
	return resolved, nil
}

func dockerConfigDirectory() (string, error) {
	if configured := os.Getenv("DOCKER_CONFIG"); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("DOCKER_CONFIG must be an absolute path")
		}
		return filepath.Clean(configured), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Docker CLI configuration directory: %w", err)
	}
	return filepath.Join(home, ".docker"), nil
}

func validateLocalDockerEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || !filepath.IsAbs(parsed.Path) || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("Docker endpoint must be a local Unix socket")
	}
	return "unix://" + filepath.Clean(parsed.Path), nil
}
