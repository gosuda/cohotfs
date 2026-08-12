// Package config defines and validates Cohotfs configuration documents.
package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	APIVersion      = "cohotfs.io/v1alpha1"
	WorkspaceKind   = "Workspace"
	HostConfigKind  = "HostConfig"
	ImagePullAlways = "always"
	ImagePullNever  = "never"
)

var workspaceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type TypeMeta struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion"`
	Kind       string `yaml:"kind" json:"kind"`
}

type HostConfig struct {
	TypeMeta       `yaml:",inline"`
	Runtime        HostRuntimeConfig     `yaml:"runtime" json:"runtime"`
	Browser        HostBrowserConfig     `yaml:"browser" json:"browser"`
	Toolchains     HostToolchainsConfig  `yaml:"toolchains" json:"toolchains"`
	Credentials    HostCredentialsConfig `yaml:"credentials" json:"credentials"`
	PermittedRoots []string              `yaml:"permittedRoots,omitempty" json:"permittedRoots,omitempty"`
	Defaults       WorkspaceDefaults     `yaml:"defaults,omitempty" json:"defaults,omitempty"`
}

type HostRuntimeConfig struct {
	Docker DockerHostConfig `yaml:"docker" json:"docker"`
}

type DockerHostConfig struct {
	Endpoint      string `yaml:"endpoint" json:"endpoint"`
	GVisorRuntime string `yaml:"gvisorRuntime" json:"gvisorRuntime"`
}

type HostBrowserConfig struct {
	LinuxExecutable   string `yaml:"linuxExecutable" json:"linuxExecutable"`
	WindowsExecutable string `yaml:"windowsExecutable" json:"windowsExecutable"`
}

type HostToolchainsConfig struct {
	GoRoot        string `yaml:"goRoot" json:"goRoot"`
	RustToolchain string `yaml:"rustToolchain" json:"rustToolchain"`
}

type HostCredentialsConfig struct {
	AgentEnvironment map[string]map[string]string `yaml:"agentEnvironment" json:"agentEnvironment"`
}

// WorkspaceDefaults contains only non-host workspace defaults. Zero-valued
// fields retain built-ins; repository configuration remains authoritative.
type WorkspaceDefaults struct {
	Runtime   RuntimeSpec  `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Resources ResourceSpec `yaml:"resources,omitempty" json:"resources,omitempty"`
}

type Workspace struct {
	TypeMeta `yaml:",inline"`
	Metadata Metadata      `yaml:"metadata" json:"metadata"`
	Spec     WorkspaceSpec `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type WorkspaceSpec struct {
	Runtime      RuntimeSpec      `yaml:"runtime" json:"runtime"`
	Image        ImageSpec        `yaml:"image" json:"image"`
	Workspace    SourceSpec       `yaml:"workspace" json:"workspace"`
	Setup        SetupSpec        `yaml:"setup" json:"setup"`
	Resources    ResourceSpec     `yaml:"resources" json:"resources"`
	Integrations IntegrationsSpec `yaml:"integrations" json:"integrations"`
}

type RuntimeSpec struct {
	Backend   string `yaml:"backend" json:"backend"`
	Isolation string `yaml:"isolation" json:"isolation"`
}

type ImageSpec struct {
	Ref        string     `yaml:"ref,omitempty" json:"ref,omitempty"`
	PullPolicy string     `yaml:"pullPolicy" json:"pullPolicy"`
	Build      *BuildSpec `yaml:"build,omitempty" json:"build,omitempty"`
}

type BuildSpec struct {
	Context       string            `yaml:"context" json:"context"`
	Containerfile string            `yaml:"containerfile" json:"containerfile"`
	Target        string            `yaml:"target" json:"target"`
	Args          map[string]string `yaml:"args" json:"args"`
}

type SourceSpec struct {
	Source string `yaml:"source" json:"source"`
	Target string `yaml:"target" json:"target"`
}

type SetupSpec struct {
	Mode    string        `yaml:"mode" json:"mode"`
	Command []string      `yaml:"command" json:"command"`
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

type ResourceSpec struct {
	Enabled    bool        `yaml:"enabled" json:"enabled"`
	CPU        float64     `yaml:"cpu" json:"cpu"`
	Memory     ByteSize    `yaml:"memory" json:"memory"`
	MemorySwap ByteSize    `yaml:"memorySwap" json:"memorySwap"`
	PIDs       int64       `yaml:"pids" json:"pids"`
	Nofile     NofileLimit `yaml:"nofile" json:"nofile"`
}

type NofileLimit struct {
	Soft uint64 `yaml:"soft" json:"soft"`
	Hard uint64 `yaml:"hard" json:"hard"`
}

type IntegrationsSpec struct {
	HostToolchains HostToolchainsSpec `yaml:"hostToolchains" json:"hostToolchains"`
	Browser        BrowserSpec        `yaml:"browser" json:"browser"`
	SSHAgent       EnabledSpec        `yaml:"sshAgent" json:"sshAgent"`
	GitCredentials GitCredentialsSpec `yaml:"gitCredentials" json:"gitCredentials"`
	Agents         AgentsSpec         `yaml:"agents" json:"agents"`
}

type EnabledSpec struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

type HostToolchainsSpec struct {
	Enabled     bool           `yaml:"enabled" json:"enabled"`
	Persistence string         `yaml:"persistence" json:"persistence"`
	RequireCOW  bool           `yaml:"requireCow" json:"requireCow"`
	Go          GoImportSpec   `yaml:"go" json:"go"`
	Rust        RustImportSpec `yaml:"rust" json:"rust"`
}

type GoImportSpec struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Root    string `yaml:"root" json:"root"`
	Caches  string `yaml:"caches" json:"caches"`
}

type RustImportSpec struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	Toolchain string `yaml:"toolchain" json:"toolchain"`
	Caches    string `yaml:"caches" json:"caches"`
}

type BrowserSpec struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Platform      string `yaml:"platform" json:"platform"`
	Executable    string `yaml:"executable" json:"executable"`
	RetainProfile bool   `yaml:"retainProfile" json:"retainProfile"`
}

type GitCredentialsSpec struct {
	Enabled         bool     `yaml:"enabled" json:"enabled"`
	AllowedContexts []string `yaml:"allowedContexts" json:"allowedContexts"`
}

type AgentsSpec struct {
	OMP    AgentSpec `yaml:"omp" json:"omp"`
	Codex  AgentSpec `yaml:"codex" json:"codex"`
	Claude AgentSpec `yaml:"claude" json:"claude"`
}

type AgentSpec struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Config  string `yaml:"config" json:"config"`
}

func BuiltinHostConfig() HostConfig {
	return HostConfig{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: HostConfigKind},
		Runtime: HostRuntimeConfig{
			Docker: DockerHostConfig{Endpoint: "auto"},
		},
		Toolchains:  HostToolchainsConfig{GoRoot: "auto", RustToolchain: "auto"},
		Credentials: HostCredentialsConfig{AgentEnvironment: map[string]map[string]string{"omp": {}, "codex": {}, "claude": {}}},
	}
}

func BuiltinWorkspace(name, imageRef string) Workspace {
	return Workspace{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: WorkspaceKind},
		Metadata: Metadata{Name: name},
		Spec: WorkspaceSpec{
			Runtime:   RuntimeSpec{Backend: "docker", Isolation: "standard"},
			Image:     ImageSpec{Ref: imageRef, PullPolicy: ImagePullAlways},
			Workspace: SourceSpec{Source: ".", Target: "/workspace"},
			Setup:     SetupSpec{Mode: "once", Command: []string{"/bin/sh", ".cohotfs/setup.sh"}, Timeout: 15 * time.Minute},
			Resources: ResourceSpec{CPU: 2, Memory: 4 << 30, MemorySwap: 5 << 30, PIDs: 512, Nofile: NofileLimit{Soft: 1024, Hard: 4096}},
			Integrations: IntegrationsSpec{
				HostToolchains: HostToolchainsSpec{Persistence: "workspace", Go: GoImportSpec{Root: "auto", Caches: "cow"}, Rust: RustImportSpec{Toolchain: "auto", Caches: "cow"}},
				Browser:        BrowserSpec{Platform: "auto"},
				Agents:         AgentsSpec{OMP: AgentSpec{Config: "seed"}, Codex: AgentSpec{Config: "seed"}, Claude: AgentSpec{Config: "seed"}},
			},
		},
	}
}

func (w Workspace) Validate() error {
	if w.APIVersion != APIVersion || w.Kind != WorkspaceKind {
		return fmt.Errorf("unsupported document %s %s", w.APIVersion, w.Kind)
	}
	if !workspaceNameRE.MatchString(w.Metadata.Name) {
		return fmt.Errorf("metadata.name must match %s", workspaceNameRE)
	}
	if w.Spec.Runtime.Backend != "docker" {
		return fmt.Errorf("spec.runtime.backend must be docker")
	}
	if w.Spec.Runtime.Isolation != "standard" && w.Spec.Runtime.Isolation != "gvisor" {
		return fmt.Errorf("spec.runtime.isolation must be standard or gvisor")
	}
	if (w.Spec.Image.Ref == "") == (w.Spec.Image.Build == nil) {
		return fmt.Errorf("exactly one of spec.image.ref and spec.image.build is required")
	}
	if w.Spec.Image.PullPolicy != ImagePullAlways && w.Spec.Image.PullPolicy != ImagePullNever {
		return fmt.Errorf("spec.image.pullPolicy must be always or never")
	}
	if w.Spec.Workspace.Source == "" || w.Spec.Workspace.Target == "" || w.Spec.Workspace.Target[0] != '/' {
		return fmt.Errorf("spec.workspace requires source and absolute target")
	}
	if w.Spec.Setup.Mode != "once" && w.Spec.Setup.Mode != "always" && w.Spec.Setup.Mode != "manual" {
		return fmt.Errorf("spec.setup.mode must be once, always, or manual")
	}
	if len(w.Spec.Setup.Command) == 0 || w.Spec.Setup.Command[0] == "" || w.Spec.Setup.Timeout <= 0 {
		return fmt.Errorf("spec.setup requires argv command and positive timeout")
	}
	if w.Spec.Resources.Enabled {
		r := w.Spec.Resources
		if r.CPU <= 0 || r.Memory <= 0 || r.MemorySwap < r.Memory || r.PIDs <= 0 || r.Nofile.Soft == 0 || r.Nofile.Hard == 0 || r.Nofile.Soft > r.Nofile.Hard {
			return fmt.Errorf("enabled resources require positive values, memorySwap >= memory, and nofile.soft <= nofile.hard")
		}
	}
	if w.Spec.Integrations.HostToolchains.Persistence != "workspace" && w.Spec.Integrations.HostToolchains.Persistence != "session" {
		return fmt.Errorf("hostToolchains.persistence must be workspace or session")
	}
	toolchains := w.Spec.Integrations.HostToolchains
	if toolchains.Go.Caches != "cow" && toolchains.Go.Caches != "isolated" {
		return fmt.Errorf("hostToolchains.go.caches must be cow or isolated")
	}
	if toolchains.Rust.Caches != "cow" && toolchains.Rust.Caches != "isolated" {
		return fmt.Errorf("hostToolchains.rust.caches must be cow or isolated")
	}
	if toolchains.Enabled && !toolchains.Go.Enabled && !toolchains.Rust.Enabled {
		return fmt.Errorf("enabled hostToolchains requires Go or Rust")
	}
	browser := w.Spec.Integrations.Browser
	if browser.Platform != "auto" && browser.Platform != "linux" && browser.Platform != "windows-wsl" {
		return fmt.Errorf("browser.platform must be auto, linux, or windows-wsl")
	}
	if browser.Executable != "" && !strings.HasPrefix(browser.Executable, "/") {
		return fmt.Errorf("browser.executable must be an absolute path")
	}
	for name, agent := range map[string]AgentSpec{"omp": w.Spec.Integrations.Agents.OMP, "codex": w.Spec.Integrations.Agents.Codex, "claude": w.Spec.Integrations.Agents.Claude} {
		if agent.Config != "seed" {
			return fmt.Errorf("agents.%s.config must be seed", name)
		}
	}
	if w.Spec.Integrations.GitCredentials.Enabled && len(w.Spec.Integrations.GitCredentials.AllowedContexts) == 0 {
		return fmt.Errorf("enabled gitCredentials requires at least one allowed context")
	}
	for _, raw := range w.Spec.Integrations.GitCredentials.AllowedContexts {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(parsed.Host, "\r\n") {
			return fmt.Errorf("invalid Git credential context %q", raw)
		}
	}
	return nil
}

func (h HostConfig) Validate() error {
	if h.APIVersion != APIVersion || h.Kind != HostConfigKind {
		return fmt.Errorf("unsupported document %s %s", h.APIVersion, h.Kind)
	}
	if h.Runtime.Docker.Endpoint == "" {
		return fmt.Errorf("runtime.docker.endpoint is required")
	}
	for agent, mappings := range h.Credentials.AgentEnvironment {
		if agent != "omp" && agent != "codex" && agent != "claude" {
			return fmt.Errorf("unknown credential agent %q", agent)
		}
		for destination, source := range mappings {
			if !allowedAgentDestination(agent, destination) || !environmentNameRE.MatchString(source) {
				return fmt.Errorf("invalid %s credential mapping %s", agent, destination)
			}
		}
	}
	return nil
}

var environmentNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func allowedAgentDestination(agent, destination string) bool {
	switch agent {
	case "omp":
		return destination == "OPENAI_API_KEY" || destination == "ANTHROPIC_API_KEY"
	case "codex":
		return destination == "OPENAI_API_KEY"
	case "claude":
		return destination == "ANTHROPIC_API_KEY"
	default:
		return false
	}
}
