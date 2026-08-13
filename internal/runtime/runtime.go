// Package runtime defines the portable Cohotfs backend boundary.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

type Capability string

const (
	CapabilityInteractiveExec Capability = "interactive_exec"
	CapabilityHostSocketBind  Capability = "host_socket_bind"
	CapabilityHostOverlay     Capability = "host_overlay"
	CapabilityBuilder         Capability = "builder"
	CapabilityRuntimeSelect   Capability = "runtime_selection"
)

type UnsupportedError struct {
	Backend    string
	Capability Capability
	Reason     string
}

func (e *UnsupportedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("backend %s does not support %s", e.Backend, e.Capability)
	}
	return fmt.Sprintf("backend %s does not support %s: %s", e.Backend, e.Capability, e.Reason)
}

func (e *UnsupportedError) Is(target error) bool {
	_, ok := target.(*UnsupportedError)
	return ok
}

var ErrUnsupported = &UnsupportedError{}

func IsUnsupported(err error) bool { return errors.Is(err, ErrUnsupported) }

type BackendInfo struct {
	Name         string              `json:"name"`
	Version      string              `json:"version"`
	Endpoint     string              `json:"endpoint"`
	Available    bool                `json:"available"`
	Capabilities map[Capability]bool `json:"capabilities"`
	Runtimes     []string            `json:"runtimes,omitempty"`
	Detail       string              `json:"detail,omitempty"`
}

type PullRequest struct {
	Reference string
	Platform  string
}

type ResolvedImage struct {
	Reference    string    `json:"reference"`
	Digest       string    `json:"digest"`
	BootstrapAPI string    `json:"bootstrapAPI"`
	ResolvedAt   time.Time `json:"resolvedAt"`
}

type Mount struct {
	Source      string
	Target      string
	ReadOnly    bool
	Propagation string
	Type        string
}

type ResourceLimits struct {
	Enabled         bool
	NanoCPUs        int64
	MemoryBytes     int64
	MemorySwapBytes int64
	PIDs            int64
	NofileSoft      uint64
	NofileHard      uint64
}

const (
	GVisorHostUDSCreate = "create"
	GVisorHostUDSAll    = "all"
)

type WorkspaceSpec struct {
	WorkspaceID    string
	OwnerUID       int
	OwnerGID       int
	ManifestDigest string
	CreationNonce  string
	Image          ResolvedImage
	Runtime        string
	GVisorHostUDS  string
	Command        []string
	Environment    []string
	Mounts         []Mount
	Resources      ResourceLimits
	Labels         map[string]string
	// Record is invoked immediately whenever the backend allocates an opaque
	// runtime ID. Orchestrators must durably persist the supplied reference.
	Record func(WorkspaceRef) error
}

// WorkspaceRef is Docker-adapter owned. Callers persist but never interpret
// the opaque container ID.
type WorkspaceRef struct {
	Backend string            `json:"backend"`
	IDs     map[string]string `json:"ids"`
	Nonce   string            `json:"nonce"`
}

type ExecRequest struct {
	Argv        []string
	Environment []string
	WorkingDir  string
	User        string
	Timeout     time.Duration
	OutputLimit int64
}

type ExecResult struct {
	ExitCode  int    `json:"exitCode"`
	Output    []byte `json:"output,omitempty"`
	Truncated bool   `json:"truncated"`
}

type WorkspaceStatus struct {
	Exists       bool              `json:"exists"`
	Running      bool              `json:"running"`
	ExitCode     int               `json:"exitCode,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreationTime time.Time         `json:"creationTime,omitempty"`
}

type Lifecycle interface {
	Probe(context.Context) (BackendInfo, error)
	Pull(context.Context, PullRequest) (ResolvedImage, error)
	Create(context.Context, WorkspaceSpec) (WorkspaceRef, error)
	Start(context.Context, WorkspaceRef) error
	ExecSync(context.Context, WorkspaceRef, ExecRequest) (ExecResult, error)
	Inspect(context.Context, WorkspaceRef) (WorkspaceStatus, error)
	Stop(context.Context, WorkspaceRef, time.Duration) error
	Delete(context.Context, WorkspaceRef) error
}

func ValidateWorkspaceSpec(spec WorkspaceSpec) error {
	if spec.WorkspaceID == "" || spec.CreationNonce == "" || spec.Image.Digest == "" {
		return fmt.Errorf("workspace ID, creation nonce, and image digest are required")
	}
	if spec.Resources.Enabled {
		resources := spec.Resources
		if resources.NanoCPUs <= 0 || resources.MemoryBytes <= 0 || resources.MemorySwapBytes < resources.MemoryBytes || resources.PIDs <= 0 || resources.NofileSoft == 0 || resources.NofileHard == 0 || resources.NofileSoft > resources.NofileHard || resources.NofileSoft > math.MaxInt64 || resources.NofileHard > math.MaxInt64 {
			return fmt.Errorf("invalid resource limits")
		}
	}
	if spec.GVisorHostUDS != "" {
		if spec.Runtime == "" || spec.GVisorHostUDS != GVisorHostUDSCreate && spec.GVisorHostUDS != GVisorHostUDSAll {
			return fmt.Errorf("invalid gVisor host UDS policy")
		}
	}
	for key, value := range map[string]string{
		"io.cohotfs.owner-uid":       strconv.Itoa(spec.OwnerUID),
		"io.cohotfs.workspace-id":    spec.WorkspaceID,
		"io.cohotfs.manifest-digest": spec.ManifestDigest,
		"io.cohotfs.creation-nonce":  spec.CreationNonce,
	} {
		if spec.Labels[key] != value {
			return fmt.Errorf("required label %s is missing or mismatched", key)
		}
	}
	return nil
}

func RecordWorkspaceRef(spec WorkspaceSpec, ref WorkspaceRef) error {
	if spec.Record == nil {
		return nil
	}
	return spec.Record(ref)
}

func CloneLabels(labels map[string]string) map[string]string {
	clone := make(map[string]string, len(labels))
	for key, value := range labels {
		clone[key] = value
	}
	return clone
}

type BuildRequest struct {
	Context        string
	Containerfile  string
	Target         string
	Args           map[string]string
	Tags           []string
	PermittedRoots []string
	CohotfsRoot    string
}

type BuildEvent struct {
	Time    time.Time `json:"time"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
	Error   string    `json:"error,omitempty"`
}

type Builder interface {
	Build(context.Context, BuildRequest) (ResolvedImage, <-chan BuildEvent, error)
}

type InteractiveRequest struct {
	Argv        []string
	Environment []string
	WorkingDir  string
	User        string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Terminal    bool
}

type InteractiveExecutor interface {
	ExecInteractive(context.Context, WorkspaceRef, InteractiveRequest) error
}
