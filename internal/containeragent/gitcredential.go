package containeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
)

func RunGitCredential(ctx context.Context, operation, socket, workspaceID string, stdin io.Reader, stdout io.Writer) error {
	if operation != "get" {
		return fmt.Errorf("Git credential operation %s is unsupported", operation)
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, (64<<10)+1))
	if err != nil {
		return err
	}
	if len(payload) > 64<<10 {
		return fmt.Errorf("Git credential request exceeds 64 KiB")
	}
	requestBody, err := json.Marshal(api.GitCredentialRequest{Protocol: api.ProtocolVersion, WorkspaceID: workspaceID, Operation: operation, Payload: payload})
	if err != nil {
		return err
	}
	client := unixHTTPClient(socket)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://cohotfs/v1/git-credential", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Git credential broker returned HTTP %d", response.StatusCode)
	}
	var result api.GitCredentialResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return err
	}
	_, err = stdout.Write(result.Payload)
	for index := range result.Payload {
		result.Payload[index] = 0
	}
	return err
}

func unixHTTPClient(socket string) *http.Client {
	transport := &http.Transport{DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{}
		return dialer.DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}
