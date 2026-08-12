package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

func decodeStrict(data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("configuration must contain exactly one document")
		}
		return err
	}
	return nil
}

func DecodeWorkspace(data []byte) (Workspace, error) {
	var meta TypeMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return Workspace{}, fmt.Errorf("parse workspace type: %w", err)
	}
	if meta.APIVersion != APIVersion || meta.Kind != WorkspaceKind {
		return Workspace{}, fmt.Errorf("unsupported document %s %s", meta.APIVersion, meta.Kind)
	}
	var workspace Workspace
	if err := decodeStrict(data, &workspace); err != nil {
		return Workspace{}, fmt.Errorf("parse workspace: %w", err)
	}
	if err := workspace.Validate(); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

func LoadWorkspace(path string) (Workspace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Workspace{}, err
	}
	return DecodeWorkspace(data)
}

func DecodeHost(data []byte) (HostConfig, error) {
	var meta TypeMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return HostConfig{}, fmt.Errorf("parse host config type: %w", err)
	}
	if meta.APIVersion != APIVersion || meta.Kind != HostConfigKind {
		return HostConfig{}, fmt.Errorf("unsupported document %s %s", meta.APIVersion, meta.Kind)
	}
	baseRaw, err := Render(BuiltinHostConfig())
	if err != nil {
		return HostConfig{}, err
	}
	baseNode, err := documentNode(baseRaw)
	if err != nil {
		return HostConfig{}, err
	}
	userNode, err := documentNode(data)
	if err != nil {
		return HostConfig{}, fmt.Errorf("parse host config: %w", err)
	}
	mergeMapping(baseNode, userNode)
	var merged bytes.Buffer
	encoder := yaml.NewEncoder(&merged)
	encoder.SetIndent(2)
	if err := encoder.Encode(baseNode); err != nil {
		return HostConfig{}, err
	}
	_ = encoder.Close()
	var host HostConfig
	if err := decodeStrict(merged.Bytes(), &host); err != nil {
		return HostConfig{}, fmt.Errorf("parse host config: %w", err)
	}
	if err := host.Validate(); err != nil {
		return HostConfig{}, err
	}
	return host, nil
}

func LoadHost(path string) (HostConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return BuiltinHostConfig(), nil
	}
	if err != nil {
		return HostConfig{}, err
	}
	return DecodeHost(data)
}

func Render(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// ProjectIdentity returns the canonical source, full digest, and the fixed
// 128-bit directory key used for machine-local overrides.
func ProjectIdentity(source string) (canonical, digest, key string, err error) {
	canonical, err = filepath.Abs(source)
	if err != nil {
		return "", "", "", err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	digest = hex.EncodeToString(sum[:])
	return canonical, digest, digest[:32], nil
}

// RedactedJSON protects future secret-bearing fields by key name. Current host
// config stores only environment variable names, never their values.
func RedactedJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	redactTree(tree)
	return json.MarshalIndent(tree, "", "  ")
}

func redactTree(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "privatekey") {
				typed[key] = "[REDACTED]"
				continue
			}
			redactTree(item)
		}
	case []any:
		for _, item := range typed {
			redactTree(item)
		}
	}
}
