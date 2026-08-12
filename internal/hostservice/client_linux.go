//go:build linux

package hostservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/proc"
)

type Client struct {
	root       *hostroot.Root
	socketPath string
	http       *http.Client
}

func NewClient(root *hostroot.Root) (*Client, error) {
	path, err := root.SocketPath(socketRelativePath)
	if err != nil {
		return nil, err
	}
	client := &Client{root: root, socketPath: path}
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if _, err := proc.ReadSocket(path, root.UID()); err != nil {
				return nil, err
			}
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "unix", path)
		},
	}
	client.http = &http.Client{Transport: transport, Timeout: 10 * time.Second}
	return client, nil
}

func (c *Client) Status(ctx context.Context) (api.HostStatus, error) {
	var status api.HostStatus
	if err := c.request(ctx, http.MethodGet, "/v1/status", nil, &status); err != nil {
		return api.HostStatus{}, err
	}
	if status.Protocol != api.ProtocolVersion || status.UID != c.root.UID() || status.Root != c.root.Path() {
		return api.HostStatus{}, fmt.Errorf("host service identity mismatch")
	}
	identity := proc.Identity{PID: status.PID, UID: status.UID, StartTicks: status.ProcessStartTicks, Executable: status.Executable, ExecutableDigest: status.ExecutableDigest}
	if err := proc.Matches(identity); err != nil {
		return api.HostStatus{}, err
	}
	return status, nil
}

func (c *Client) Stop(ctx context.Context, force bool) error {
	return c.request(ctx, http.MethodPost, "/v1/stop", api.StopRequest{Protocol: api.ProtocolVersion, Force: force}, nil)
}

func (c *Client) Acquire(ctx context.Context, request api.LeaseRequest) (api.LeaseResponse, error) {
	request.Protocol = api.ProtocolVersion
	var response api.LeaseResponse
	if err := c.request(ctx, http.MethodPost, "/v1/lease", request, &response); err != nil {
		return api.LeaseResponse{}, err
	}
	return response, nil
}

func (c *Client) Release(ctx context.Context, request api.ReleaseRequest) error {
	request.Protocol = api.ProtocolVersion
	return c.request(ctx, http.MethodPost, "/v1/release", request, nil)
}

func (c *Client) GitCredential(ctx context.Context, request api.GitCredentialRequest) ([]byte, error) {
	request.Protocol = api.ProtocolVersion
	var response api.GitCredentialResponse
	if err := c.request(ctx, http.MethodPost, "/v1/git-credential", request, &response); err != nil {
		return nil, err
	}
	return response.Payload, nil
}

func (c *Client) IssueAgentSecret(ctx context.Context, request api.AgentSecretIssueRequest) (api.AgentSecretIssueResponse, error) {
	request.Protocol = api.ProtocolVersion
	var response api.AgentSecretIssueResponse
	if err := c.request(ctx, http.MethodPost, "/v1/agent-secret/issue", request, &response); err != nil {
		return api.AgentSecretIssueResponse{}, err
	}
	return response, nil
}

func (c *Client) FetchAgentSecret(ctx context.Context, request api.AgentSecretFetchRequest) (api.AgentSecretFetchResponse, error) {
	request.Protocol = api.ProtocolVersion
	var response api.AgentSecretFetchResponse
	if err := c.request(ctx, http.MethodPost, "/v1/agent-secret/fetch", request, &response); err != nil {
		return api.AgentSecretFetchResponse{}, err
	}
	return response, nil
}

func (c *Client) request(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(body); err != nil {
			return err
		}
		reader = &buffer
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://cohotfs"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope api.LeaseResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&envelope)
		if envelope.Error != nil {
			return fmt.Errorf("host service %s: %s", envelope.Error.Code, envelope.Error.Message)
		}
		return fmt.Errorf("host service returned HTTP %d", response.StatusCode)
	}
	if result != nil {
		decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(result); err != nil {
			return err
		}
	}
	return nil
}

// EnsureRunning validates an existing host process or spawns this executable
// with fixed argv and a sanitized environment, then waits for identity-checked
// readiness.
func EnsureRunning(ctx context.Context, root *hostroot.Root) (*Client, error) {
	client, err := NewClient(root)
	if err != nil {
		return nil, err
	}
	if _, err := client.Status(ctx); err == nil {
		return client, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	logPath, _ := root.HostPath("logs/host-process.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "host", "serve")
	command.Env = proc.SanitizedEnvironment(os.Environ())
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	_ = command.Process.Release()
	_ = logFile.Close()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("host service readiness timed out")
		case <-ticker.C:
			status, statusErr := client.Status(ctx)
			if statusErr == nil && status.Executable == executable {
				return client, nil
			}
		}
	}
}

func IsUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
