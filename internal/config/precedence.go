package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

type WorkspaceFlags struct {
	Name      *string
	Backend   *string
	Isolation *string
	ImageRef  *string
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
