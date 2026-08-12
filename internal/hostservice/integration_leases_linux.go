//go:build linux

package hostservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/integration"
	"github.com/gosuda/cohotfs/internal/proc"
)

func (s *Server) acquireGitCredential(_ context.Context, request api.LeaseRequest) (Lease, api.LeaseResponse, error) {
	var allowedContexts []string
	if err := json.Unmarshal([]byte(request.Parameters["allowedContexts"]), &allowedContexts); err != nil {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("invalid Git credential lease policy: %w", err)
	}
	if err := integration.ValidateCredentialContexts(allowedContexts); err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/git-credential", s.authorize(func(response http.ResponseWriter, incoming *http.Request) {
		var credential api.GitCredentialRequest
		if err := decodeJSON(incoming, &credential); err != nil || credential.Protocol != api.ProtocolVersion || credential.WorkspaceID != request.WorkspaceID {
			writeAPIError(response, http.StatusBadRequest, "request", "invalid Git credential request")
			return
		}
		payload, err := integration.FillCredential(incoming.Context(), credential.Operation, credential.Payload, allowedContexts, integration.GitProvider{})
		if err != nil {
			writeAPIError(response, http.StatusForbidden, "credential_policy", err.Error())
			return
		}
		writeJSON(response, http.StatusOK, api.GitCredentialResponse{Payload: payload})
	}))
	socket, err := s.newLeaseHTTPServer(filepath.Join("run", "workspaces", request.WorkspaceID, "git.sock"), mux)
	if err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	lease := Lease{Summary: api.LeaseSummary{WorkspaceID: request.WorkspaceID, Kind: api.LeaseGitCredential}, Sockets: []proc.SocketIdentity{socket.identity}, Close: socket.close}
	return lease, api.LeaseResponse{Endpoint: "/run/cohotfs/host/git.sock", Metadata: socketLeaseMetadata(socket.identity)}, nil
}

func (s *Server) acquireAgentSecretGateway(_ context.Context, request api.LeaseRequest) (Lease, api.LeaseResponse, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent-secret/fetch", s.authorize(func(response http.ResponseWriter, incoming *http.Request) {
		var fetch api.AgentSecretFetchRequest
		if err := decodeJSON(incoming, &fetch); err != nil || fetch.Protocol != api.ProtocolVersion || fetch.WorkspaceID != request.WorkspaceID || fetch.LeaseID == "" {
			writeAPIError(response, http.StatusBadRequest, "request", "invalid agent secret fetch")
			return
		}
		destination, value, err := s.secrets.Fetch(fetch.LeaseID, request.WorkspaceID)
		if err != nil {
			writeAPIError(response, http.StatusGone, "secret_unavailable", err.Error())
			return
		}
		defer zeroBytes(value)
		writeJSON(response, http.StatusOK, api.AgentSecretFetchResponse{Destination: destination, Value: value})
	}))
	socket, err := s.newLeaseHTTPServer(filepath.Join("run", "workspaces", request.WorkspaceID, "secret.sock"), mux)
	if err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	lease := Lease{Summary: api.LeaseSummary{WorkspaceID: request.WorkspaceID, Kind: api.LeaseAgentSecret}, Sockets: []proc.SocketIdentity{socket.identity}, Close: socket.close}
	return lease, api.LeaseResponse{Endpoint: "/run/cohotfs/host/secret.sock", Metadata: socketLeaseMetadata(socket.identity)}, nil
}
func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
