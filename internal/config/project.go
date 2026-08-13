package config

import (
	"fmt"
	"os"
)

const ProjectConfigKind = "ProjectConfig"

// ProjectDocument binds one trusted, host-owned workspace configuration to its
// canonical source directory. Repository files never participate in resolution.
type ProjectDocument struct {
	TypeMeta     `yaml:",inline"`
	SourcePath   string    `yaml:"sourcePath" json:"sourcePath"`
	SourceDigest string    `yaml:"sourceDigest" json:"sourceDigest"`
	Workspace    Workspace `yaml:"workspace" json:"workspace"`
}

func NewProjectDocument(source string, workspace Workspace) (ProjectDocument, error) {
	canonical, digest, _, err := ProjectIdentity(source)
	if err != nil {
		return ProjectDocument{}, err
	}
	if err := workspace.Validate(); err != nil {
		return ProjectDocument{}, err
	}
	return ProjectDocument{
		TypeMeta:     TypeMeta{APIVersion: APIVersion, Kind: ProjectConfigKind},
		SourcePath:   canonical,
		SourceDigest: digest,
		Workspace:    workspace,
	}, nil
}

func DecodeProject(data []byte, source string) (ProjectDocument, error) {
	var document ProjectDocument
	if err := decodeStrict(data, &document); err != nil {
		return ProjectDocument{}, fmt.Errorf("parse project config: %w", err)
	}
	canonical, digest, _, err := ProjectIdentity(source)
	if err != nil {
		return ProjectDocument{}, err
	}
	if document.APIVersion != APIVersion || document.Kind != ProjectConfigKind {
		return ProjectDocument{}, fmt.Errorf("unsupported document %s %s", document.APIVersion, document.Kind)
	}
	if document.SourcePath != canonical || document.SourceDigest != digest {
		return ProjectDocument{}, fmt.Errorf("project config identity mismatch")
	}
	if err := document.Workspace.Validate(); err != nil {
		return ProjectDocument{}, err
	}
	return document, nil
}

func LoadProject(path, source string) (ProjectDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ProjectDocument{}, err
	}
	return DecodeProject(data, source)
}
