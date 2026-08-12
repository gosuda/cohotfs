//go:build linux

// Package hostservice implements the optional same-UID HTTP-over-Unix host
// process used only for persistent integration leases.
package hostservice

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/config"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/integration"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/reconcile"
	"golang.org/x/sys/unix"
)

const socketRelativePath = "run/host.sock"

type peerUIDKey struct{}

type Lease struct {
	Summary        api.LeaseSummary
	IdempotencyKey string
	RequestDigest  string
	Response       api.LeaseResponse
	Processes      []ManagedProcess
	Sockets        []proc.SocketIdentity
	Mounts         []reconcile.MountIdentity
	Close          func(context.Context) error
}

type AcquireFunc func(context.Context, api.LeaseRequest) (Lease, api.LeaseResponse, error)

type Server struct {
	root       *hostroot.Root
	identity   proc.Identity
	hostConfig config.HostConfig
	startedAt  time.Time
	listener   *net.UnixListener
	httpServer *http.Server
	stop       chan struct{}
	stopOnce   sync.Once

	mu       sync.Mutex
	leases   map[string]Lease
	handlers map[api.LeaseKind]AcquireFunc
	secrets  *integration.SecretStore
}

func NewServer(root *hostroot.Root) (*Server, error) {
	identity, err := proc.CurrentIdentity()
	if err != nil {
		return nil, err
	}
	configPath, _ := root.HostPath("config.yaml")
	hostConfig, err := config.LoadHost(configPath)
	if err != nil {
		return nil, err
	}
	server := &Server{
		root:       root,
		identity:   identity,
		startedAt:  time.Now().UTC(),
		stop:       make(chan struct{}),
		leases:     make(map[string]Lease),
		handlers:   make(map[api.LeaseKind]AcquireFunc),
		secrets:    integration.NewSecretStore(),
		hostConfig: hostConfig,
	}
	server.Register(api.LeaseChrome, server.acquireChrome)
	server.Register(api.LeaseGitCredential, server.acquireGitCredential)
	server.Register(api.LeaseAgentSecret, server.acquireAgentSecretGateway)
	return server, nil
}

func (s *Server) Register(kind api.LeaseKind, acquire AcquireFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = acquire
}

func (s *Server) Serve(ctx context.Context) error {
	if err := s.reconcileStaleLeases(ctx); err != nil {
		return err
	}
	path, err := s.root.SocketPath(socketRelativePath)
	if err != nil {
		return err
	}
	if err := removeStaleSocket(path, s.root.UID()); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	s.listener = listener
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return err
	}
	socketIdentity, err := proc.ReadSocket(path, s.root.UID())
	if err != nil {
		_ = listener.Close()
		return err
	}
	if err := s.persistRecord(socketIdentity); err != nil {
		_ = listener.Close()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.authorize(s.handleStatus))
	mux.HandleFunc("POST /v1/lease", s.authorize(s.handleLease))
	mux.HandleFunc("POST /v1/release", s.authorize(s.handleRelease))
	mux.HandleFunc("POST /v1/stop", s.authorize(s.handleStop))
	mux.HandleFunc("POST /v1/agent-secret/issue", s.authorize(s.handleAgentSecretIssue))
	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			uid, err := peerUID(connection)
			if err != nil {
				uid = -1
			}
			return context.WithValue(ctx, peerUIDKey{}, uid)
		},
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.httpServer.Serve(listener) }()
	var result error
	received := false
	select {
	case <-ctx.Done():
		s.stopOnce.Do(func() { close(s.stop) })
	case <-s.stop:
	case result = <-serveErr:
		received = true
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.drain(shutdownCtx)
	_ = s.httpServer.Shutdown(shutdownCtx)
	if !received {
		result = <-serveErr
	}
	s.cleanup(socketIdentity)
	if errors.Is(result, http.ErrServerClosed) || errors.Is(result, net.ErrClosed) {
		return nil
	}
	return result
}

func removeStaleSocket(path string, uid int) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("host socket path contains an unrecognized object")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("host socket identity is unsafe")
	}
	connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("host service is already running")
	}
	return os.Remove(path)
}

func (s *Server) persistRecord(socket proc.SocketIdentity) error {
	record := struct {
		Protocol string              `json:"protocol"`
		Root     string              `json:"root"`
		Process  proc.Identity       `json:"process"`
		Socket   proc.SocketIdentity `json:"socket"`
	}{api.ProtocolVersion, s.root.Path(), s.identity, socket}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return s.root.AtomicWrite("run/host.json", append(data, '\n'), 0o600)
}

func (s *Server) cleanup(socket proc.SocketIdentity) {
	if proc.ValidateSocket(socket) == nil {
		_ = os.Remove(socket.Path)
	}
	_ = s.root.Remove("run/host.json")
}

func (s *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		uid, _ := request.Context().Value(peerUIDKey{}).(int)
		if uid != s.root.UID() {
			writeAPIError(response, http.StatusForbidden, "peer_uid", "peer UID is not authorized")
			return
		}
		next(response, request)
	}
}

func (s *Server) status() api.HostStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	leases := make([]api.LeaseSummary, 0, len(s.leases))
	for _, lease := range s.leases {
		leases = append(leases, lease.Summary)
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].LeaseID < leases[j].LeaseID })
	return api.HostStatus{
		Protocol: api.ProtocolVersion, PID: s.identity.PID, UID: s.identity.UID,
		Root: s.root.Path(), Executable: s.identity.Executable, ExecutableDigest: s.identity.ExecutableDigest,
		ProcessStartTicks: s.identity.StartTicks, StartedAt: s.startedAt, Leases: leases,
	}
}

func (s *Server) handleStatus(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.status())
}

func (s *Server) handleLease(response http.ResponseWriter, request *http.Request) {
	var leaseRequest api.LeaseRequest
	if err := decodeJSON(request, &leaseRequest); err != nil {
		writeAPIError(response, http.StatusBadRequest, "request", err.Error())
		return
	}
	if leaseRequest.Protocol != api.ProtocolVersion || leaseRequest.WorkspaceID == "" || leaseRequest.IdempotencyKey == "" {
		writeAPIError(response, http.StatusBadRequest, "request", "protocol, workspaceID, and idempotencyKey are required")
		return
	}
	requestBody, _ := json.Marshal(leaseRequest)
	digest := fmt.Sprintf("%x", sha256.Sum256(requestBody))
	s.mu.Lock()
	for _, existing := range s.leases {
		if existing.Summary.WorkspaceID == leaseRequest.WorkspaceID && existing.IdempotencyKey == leaseRequest.IdempotencyKey {
			if existing.RequestDigest != digest {
				s.mu.Unlock()
				writeAPIError(response, http.StatusConflict, "idempotency_conflict", "idempotency key was used with different input")
				return
			}
			result := existing.Response
			s.mu.Unlock()
			writeJSON(response, http.StatusOK, result)
			return
		}
	}
	handler := s.handlers[leaseRequest.Kind]
	s.mu.Unlock()
	if handler == nil {
		writeAPIError(response, http.StatusNotImplemented, "unsupported", "lease kind is not available")
		return
	}
	lease, result, err := handler(request.Context(), leaseRequest)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, "lease_failed", err.Error())
		return
	}
	if lease.Summary.LeaseID == "" {
		lease.Summary.LeaseID = newLeaseID()
	}
	lease.Summary.WorkspaceID = leaseRequest.WorkspaceID
	lease.Summary.Kind = leaseRequest.Kind
	result.LeaseID = lease.Summary.LeaseID
	result.ExpiresAt = lease.Summary.ExpiresAt
	lease.IdempotencyKey = leaseRequest.IdempotencyKey
	lease.RequestDigest = digest
	lease.Response = result
	s.mu.Lock()
	s.leases[lease.Summary.LeaseID] = lease
	if err := s.persistLeasesLocked(); err != nil {
		delete(s.leases, lease.Summary.LeaseID)
		s.mu.Unlock()
		if lease.Close != nil {
			_ = lease.Close(request.Context())
		}
		writeAPIError(response, http.StatusInternalServerError, "lease_persistence", err.Error())
		return
	}
	s.mu.Unlock()
	writeJSON(response, http.StatusCreated, result)
}

func (s *Server) handleRelease(response http.ResponseWriter, request *http.Request) {
	var release api.ReleaseRequest
	if err := decodeJSON(request, &release); err != nil || release.Protocol != api.ProtocolVersion {
		writeAPIError(response, http.StatusBadRequest, "request", "invalid release request")
		return
	}
	s.mu.Lock()
	lease, ok := s.leases[release.LeaseID]
	if ok && lease.Summary.WorkspaceID != release.WorkspaceID {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		writeAPIError(response, http.StatusNotFound, "not_found", "lease not found")
		return
	}
	if lease.Close != nil {
		if err := lease.Close(request.Context()); err != nil {
			writeAPIError(response, http.StatusBadGateway, "cleanup", err.Error())
			return
		}
	}
	s.mu.Lock()
	delete(s.leases, release.LeaseID)
	persistErr := s.persistLeasesLocked()
	s.mu.Unlock()
	if persistErr != nil {
		writeAPIError(response, http.StatusInternalServerError, "lease_persistence", persistErr.Error())
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStop(response http.ResponseWriter, request *http.Request) {
	var stop api.StopRequest
	if err := decodeJSON(request, &stop); err != nil || stop.Protocol != api.ProtocolVersion {
		writeAPIError(response, http.StatusBadRequest, "request", "invalid stop request")
		return
	}
	s.mu.Lock()
	count := len(s.leases)
	s.mu.Unlock()
	if count != 0 && !stop.Force {
		writeAPIError(response, http.StatusConflict, "leases_active", "host service has active leases")
		return
	}
	if stop.Force {
		if err := s.drain(request.Context()); err != nil {
			writeAPIError(response, http.StatusBadGateway, "cleanup", err.Error())
			return
		}
	}
	response.WriteHeader(http.StatusNoContent)
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *Server) handleAgentSecretIssue(response http.ResponseWriter, request *http.Request) {
	var issue api.AgentSecretIssueRequest
	if err := decodeJSON(request, &issue); err != nil || issue.Protocol != api.ProtocolVersion || issue.WorkspaceID == "" || issue.Agent == "" || issue.SourceVariable == "" || len(issue.Value) == 0 {
		writeAPIError(response, http.StatusBadRequest, "request", "invalid agent secret request")
		return
	}
	defer zeroBytes(issue.Value)
	configuredSource, allowed := s.hostConfig.Credentials.AgentEnvironment[issue.Agent][issue.Destination]
	if !allowed || configuredSource != issue.SourceVariable {
		writeAPIError(response, http.StatusForbidden, "secret_policy", "agent credential mapping is not configured")
		return
	}
	leaseID, expiresAt, err := s.secrets.Issue(issue.WorkspaceID, issue.Agent, issue.Destination, issue.Value, 60*time.Second)
	if err != nil {
		writeAPIError(response, http.StatusForbidden, "secret_policy", err.Error())
		return
	}
	writeJSON(response, http.StatusCreated, api.AgentSecretIssueResponse{LeaseID: leaseID, ExpiresAt: expiresAt})
}

func (s *Server) drain(ctx context.Context) error {
	s.mu.Lock()
	leases := make([]Lease, 0, len(s.leases))
	for _, lease := range s.leases {
		leases = append(leases, lease)
	}
	s.mu.Unlock()
	var failures []string
	for _, lease := range leases {
		if lease.Close != nil {
			if err := lease.Close(ctx); err != nil {
				failures = append(failures, err.Error())
				continue
			}
		}
		s.mu.Lock()
		delete(s.leases, lease.Summary.LeaseID)
		s.mu.Unlock()
	}
	s.mu.Lock()
	persistErr := s.persistLeasesLocked()
	s.mu.Unlock()
	if persistErr != nil {
		failures = append(failures, persistErr.Error())
	}
	if len(failures) != 0 {
		return fmt.Errorf("lease cleanup failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func peerUID(connection net.Conn) (int, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return -1, fmt.Errorf("connection is not Unix")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			controlErr = err
			return
		}
		uid = int(credential.Uid)
	}); err != nil {
		return -1, err
	}
	return uid, controlErr
}

func decodeJSON(request *http.Request, value any) error {
	raw, err := io.ReadAll(io.LimitReader(request.Body, (64<<10)+1))
	if err != nil {
		return err
	}
	if len(raw) > 64<<10 {
		return fmt.Errorf("request exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("request must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeAPIError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, api.LeaseResponse{Error: &api.Error{Code: code, Message: message}})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func newLeaseID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
}
