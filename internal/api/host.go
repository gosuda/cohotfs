// Package api defines the small versioned protocol used only by the optional
// per-user host process and its integration leases.
package api

import "time"

const ProtocolVersion = "v1alpha1"

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type LeaseKind string

const (
	LeaseChrome        LeaseKind = "chrome"
	LeaseGitCredential LeaseKind = "git_credential"
	LeaseAgentSecret   LeaseKind = "agent_secret"
	LeaseOverlay       LeaseKind = "overlay"
)

type LeaseRequest struct {
	Protocol       string            `json:"protocol"`
	IdempotencyKey string            `json:"idempotencyKey"`
	WorkspaceID    string            `json:"workspaceID"`
	Kind           LeaseKind         `json:"kind"`
	Parameters     map[string]string `json:"parameters,omitempty"`
}

type LeaseResponse struct {
	LeaseID   string            `json:"leaseID,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Error     *Error            `json:"error,omitempty"`
}

type ReleaseRequest struct {
	Protocol    string `json:"protocol"`
	WorkspaceID string `json:"workspaceID"`
	LeaseID     string `json:"leaseID"`
}

type GitCredentialRequest struct {
	Protocol    string `json:"protocol"`
	WorkspaceID string `json:"workspaceID"`
	Operation   string `json:"operation"`
	Payload     []byte `json:"payload"`
}

type GitCredentialResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

type AgentSecretIssueRequest struct {
	Protocol       string `json:"protocol"`
	WorkspaceID    string `json:"workspaceID"`
	Agent          string `json:"agent"`
	Destination    string `json:"destination"`
	SourceVariable string `json:"sourceVariable"`
	Value          []byte `json:"value"`
}

type AgentSecretIssueResponse struct {
	LeaseID   string    `json:"leaseID,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	Error     *Error    `json:"error,omitempty"`
}

type AgentSecretFetchRequest struct {
	Protocol    string `json:"protocol"`
	WorkspaceID string `json:"workspaceID"`
	LeaseID     string `json:"leaseID"`
}

type AgentSecretFetchResponse struct {
	Destination string `json:"destination,omitempty"`
	Value       []byte `json:"value,omitempty"`
	Error       *Error `json:"error,omitempty"`
}

type HostStatus struct {
	Protocol          string         `json:"protocol"`
	PID               int            `json:"pid"`
	UID               int            `json:"uid"`
	Root              string         `json:"root"`
	Executable        string         `json:"executable"`
	ExecutableDigest  string         `json:"executableDigest"`
	ProcessStartTicks uint64         `json:"processStartTicks"`
	StartedAt         time.Time      `json:"startedAt"`
	Leases            []LeaseSummary `json:"leases"`
}

type StopRequest struct {
	Protocol string `json:"protocol"`
	Force    bool   `json:"force"`
}

type LeaseSummary struct {
	LeaseID     string    `json:"leaseID"`
	WorkspaceID string    `json:"workspaceID"`
	Kind        LeaseKind `json:"kind"`
	ExpiresAt   time.Time `json:"expiresAt,omitempty"`
}
