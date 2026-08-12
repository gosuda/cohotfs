package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

type WorkspaceFlags struct {
	Name      *string
	Backend   *string
	Isolation *string
	ImageRef  *string
}

type projectOverrideDocument struct {
	TypeMeta     `yaml:",inline"`
	SourcePath   string    `yaml:"sourcePath"`
	SourceDigest string    `yaml:"sourceDigest"`
	Workspace    yaml.Node `yaml:"workspace,omitempty"`
}

// ResolveWorkspace applies built-ins, host defaults, repository configuration,
// machine-local override, then explicit flags. Unknown keys are rejected after
// merge, so a partial higher-precedence document cannot smuggle new fields.
func ResolveWorkspace(manifestPath, overridePath string, host HostConfig, flags WorkspaceFlags, imageRef string) (Workspace, error) {
	projectRoot := filepath.Dir(filepath.Dir(manifestPath))
	canonical, digest, _, err := ProjectIdentity(projectRoot)
	if err != nil {
		return Workspace{}, err
	}
	name := filepath.Base(canonical)
	base := BuiltinWorkspace(name, imageRef)
	applyHostDefaults(&base, host.Defaults)
	baseRaw, err := Render(base)
	if err != nil {
		return Workspace{}, err
	}
	baseNode, err := documentNode(baseRaw)
	if err != nil {
		return Workspace{}, err
	}

	repositoryRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Workspace{}, err
	}
	var repositoryMeta TypeMeta
	if err := yaml.Unmarshal(repositoryRaw, &repositoryMeta); err != nil {
		return Workspace{}, err
	}
	if repositoryMeta.APIVersion != APIVersion || repositoryMeta.Kind != WorkspaceKind {
		return Workspace{}, fmt.Errorf("unsupported document %s %s", repositoryMeta.APIVersion, repositoryMeta.Kind)
	}
	repositoryNode, err := documentNode(repositoryRaw)
	if err != nil {
		return Workspace{}, err
	}
	mergeMapping(baseNode, repositoryNode)

	if overridePath != "" {
		overrideRaw, readErr := os.ReadFile(overridePath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return Workspace{}, readErr
		}
		if readErr == nil {
			var override projectOverrideDocument
			if err := decodeStrict(overrideRaw, &override); err != nil {
				return Workspace{}, fmt.Errorf("parse project override: %w", err)
			}
			if override.APIVersion != APIVersion || override.Kind != "ProjectOverride" || override.SourcePath != canonical || override.SourceDigest != digest {
				return Workspace{}, fmt.Errorf("project override identity mismatch")
			}
			if override.Workspace.Kind != 0 {
				if override.Workspace.Kind != yaml.MappingNode {
					return Workspace{}, fmt.Errorf("project override workspace must be a mapping")
				}
				mergeMapping(baseNode, &override.Workspace)
			}
		}
	}

	var merged bytes.Buffer
	encoder := yaml.NewEncoder(&merged)
	encoder.SetIndent(2)
	if err := encoder.Encode(baseNode); err != nil {
		return Workspace{}, err
	}
	_ = encoder.Close()
	workspace, err := DecodeWorkspace(merged.Bytes())
	if err != nil {
		return Workspace{}, err
	}
	applyFlags(&workspace, flags)
	if err := workspace.Validate(); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

// ResolveDefaultWorkspace applies built-ins and host defaults without requiring
// a repository manifest. It is used by the zero-argument CLI for an uninitialized
// current directory.
func ResolveDefaultWorkspace(name string, host HostConfig, flags WorkspaceFlags, imageRef string) (Workspace, error) {
	workspace := BuiltinWorkspace(name, imageRef)
	applyHostDefaults(&workspace, host.Defaults)
	applyFlags(&workspace, flags)
	if err := workspace.Validate(); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

func documentNode(data []byte) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("configuration document must be a mapping")
	}
	return document.Content[0], nil
}

func mergeMapping(base, overlay *yaml.Node) {
	for index := 0; index < len(overlay.Content); index += 2 {
		key, value := overlay.Content[index], overlay.Content[index+1]
		baseIndex := mappingIndex(base, key.Value)
		if baseIndex >= 0 && base.Content[baseIndex+1].Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			mergeMapping(base.Content[baseIndex+1], value)
			continue
		}
		if baseIndex >= 0 {
			base.Content[baseIndex] = cloneNode(key)
			base.Content[baseIndex+1] = cloneNode(value)
		} else {
			base.Content = append(base.Content, cloneNode(key), cloneNode(value))
		}
	}
}

func mappingIndex(mapping *yaml.Node, key string) int {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return index
		}
	}
	return -1
}

func cloneNode(node *yaml.Node) *yaml.Node {
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		clone.Content[index] = cloneNode(child)
	}
	return &clone
}

func applyHostDefaults(workspace *Workspace, defaults WorkspaceDefaults) {
	if defaults.Runtime.Backend != "" {
		workspace.Spec.Runtime.Backend = defaults.Runtime.Backend
	}
	if defaults.Runtime.Isolation != "" {
		workspace.Spec.Runtime.Isolation = defaults.Runtime.Isolation
	}
	if defaults.Resources.CPU != 0 {
		workspace.Spec.Resources = defaults.Resources
	}
}

func applyFlags(workspace *Workspace, flags WorkspaceFlags) {
	if flags.Name != nil {
		workspace.Metadata.Name = *flags.Name
	}
	if flags.Backend != nil {
		workspace.Spec.Runtime.Backend = *flags.Backend
	}
	if flags.Isolation != nil {
		workspace.Spec.Runtime.Isolation = *flags.Isolation
	}
	if flags.ImageRef != nil {
		workspace.Spec.Image.Ref = *flags.ImageRef
		workspace.Spec.Image.Build = nil
	}
}
