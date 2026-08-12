//go:build linux

package containeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/gosuda/cohotfs/internal/api"
	"golang.org/x/sys/unix"
)

func RunAgent(ctx context.Context, socket, workspaceID string, leaseIDs, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !workspaceIDPattern.MatchString(workspaceID) || len(argv) == 0 {
		return fmt.Errorf("agent-run requires a workspace ID and command")
	}
	environment := os.Environ()
	secrets := make([][]byte, 0, len(leaseIDs))
	defer func() {
		for _, value := range secrets {
			_ = unix.Munlock(value)
			zeroAgentBytes(value)
		}
	}()
	seen := map[string]bool{}
	for _, leaseID := range leaseIDs {
		response, err := fetchAgentSecret(ctx, socket, workspaceID, leaseID)
		if err != nil {
			return err
		}
		if !allowedAgentEnvironmentDestination(response.Destination) || seen[response.Destination] || len(response.Value) == 0 || bytes.IndexByte(response.Value, 0) >= 0 {
			zeroAgentBytes(response.Value)
			return fmt.Errorf("invalid agent secret response")
		}
		seen[response.Destination] = true
		_ = unix.Mlock(response.Value)
		secrets = append(secrets, response.Value)
		environment = replaceEnvironment(environment, response.Destination, string(response.Value))
	}
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, argv[1:]...)
	command.Env = environment
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func fetchAgentSecret(ctx context.Context, socket, workspaceID, leaseID string) (api.AgentSecretFetchResponse, error) {
	requestBody := api.AgentSecretFetchRequest{Protocol: api.ProtocolVersion, WorkspaceID: workspaceID, LeaseID: leaseID}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return api.AgentSecretFetchResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://cohotfs/v1/agent-secret/fetch", bytes.NewReader(raw))
	if err != nil {
		return api.AgentSecretFetchResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := unixHTTPClient(socket).Do(request)
	if err != nil {
		return api.AgentSecretFetchResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var body api.AgentSecretFetchResponse
		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body)
		if body.Error != nil {
			return api.AgentSecretFetchResponse{}, fmt.Errorf("agent secret broker: %s", body.Error.Message)
		}
		return api.AgentSecretFetchResponse{}, fmt.Errorf("agent secret broker returned HTTP %d", response.StatusCode)
	}
	var result api.AgentSecretFetchResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return api.AgentSecretFetchResponse{}, err
	}
	return result, nil
}

func allowedAgentEnvironmentDestination(destination string) bool {
	return destination == "OPENAI_API_KEY" || destination == "ANTHROPIC_API_KEY"
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := environment[:0]
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func zeroAgentBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
