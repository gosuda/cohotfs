package integration

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

type CredentialRequest struct {
	Protocol string
	Host     string
	Path     string
	Fields   map[string]string
}

type CredentialProvider interface {
	Fill(context.Context, []byte) ([]byte, error)
}

type GitProvider struct{}

func (GitProvider) Fill(ctx context.Context, input []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", "credential", "fill")
	command.Stdin = bytes.NewReader(input)
	return command.Output()
}

func ParseCredentialRequest(input []byte) (CredentialRequest, error) {
	if len(input) > 64<<10 {
		return CredentialRequest{}, fmt.Errorf("credential request exceeds 64 KiB")
	}
	request := CredentialRequest{Fields: map[string]string{}}
	scanner := bufio.NewScanner(bytes.NewReader(input))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n\x00") {
			return CredentialRequest{}, fmt.Errorf("malformed Git credential line")
		}
		if _, duplicate := request.Fields[key]; duplicate {
			return CredentialRequest{}, fmt.Errorf("duplicate Git credential field %s", key)
		}
		request.Fields[key] = value
	}
	if err := scanner.Err(); err != nil {
		return CredentialRequest{}, err
	}
	request.Protocol, request.Host, request.Path = request.Fields["protocol"], request.Fields["host"], request.Fields["path"]
	if request.Protocol != "http" && request.Protocol != "https" || request.Host == "" {
		return CredentialRequest{}, fmt.Errorf("Git credential protocol and host are required")
	}
	return request, nil
}

func ValidateCredentialContexts(allowed []string) error {
	if len(allowed) == 0 {
		return fmt.Errorf("at least one Git credential context is required")
	}
	for _, raw := range allowed {
		contextURL, err := url.Parse(raw)
		if err != nil || (contextURL.Scheme != "http" && contextURL.Scheme != "https") || contextURL.Host == "" || contextURL.User != nil || contextURL.RawQuery != "" || contextURL.Fragment != "" || strings.ContainsAny(contextURL.Host, "\r\n") {
			return fmt.Errorf("invalid Git credential context %q", raw)
		}
	}
	return nil
}

func AuthorizeCredential(request CredentialRequest, allowed []string) bool {
	requestURL := &url.URL{Scheme: request.Protocol, Host: request.Host, Path: "/" + strings.TrimPrefix(request.Path, "/")}
	for _, raw := range allowed {
		contextURL, err := url.Parse(raw)
		if err != nil || contextURL.Scheme == "" || contextURL.Host == "" || contextURL.RawQuery != "" || contextURL.Fragment != "" {
			continue
		}
		if contextURL.Scheme != requestURL.Scheme || !strings.EqualFold(contextURL.Hostname(), requestURL.Hostname()) || contextURL.Port() != requestURL.Port() {
			continue
		}
		contextPath := strings.TrimSuffix(contextURL.EscapedPath(), "/")
		requestPath := strings.TrimSuffix(requestURL.EscapedPath(), "/")
		if contextPath == requestPath {
			return true
		}
	}
	return false
}

func FillCredential(ctx context.Context, operation string, input []byte, allowed []string, provider CredentialProvider) ([]byte, error) {
	if operation != "get" {
		return nil, fmt.Errorf("Git credential operation %s is unsupported", operation)
	}
	request, err := ParseCredentialRequest(input)
	if err != nil {
		return nil, err
	}
	if !AuthorizeCredential(request, allowed) {
		return nil, fmt.Errorf("Git credential context is not allowlisted")
	}
	providerContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := provider.Fill(providerContext, input)
	if err != nil {
		return nil, fmt.Errorf("Git credential provider: %w", err)
	}
	defer zeroCredentialBytes(output)
	if len(output) > 64<<10 {
		return nil, fmt.Errorf("Git credential response exceeds 64 KiB")
	}
	fields, err := ParseCredentialResponse(output)
	if err != nil {
		return nil, err
	}
	var scrubbed bytes.Buffer
	for _, key := range []string{"username", "password", "password_expiry_utc"} {
		if value := fields[key]; value != "" {
			fmt.Fprintf(&scrubbed, "%s=%s\n", key, value)
		}
	}
	scrubbed.WriteByte('\n')
	return scrubbed.Bytes(), nil
}

func ParseCredentialResponse(input []byte) (map[string]string, error) {
	fields := map[string]string{}
	reader := bufio.NewReader(io.LimitReader(bytes.NewReader(input), 64<<10+1))
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" && (err == nil || err == io.EOF) {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("malformed Git credential response")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("duplicate Git credential response field %s", key)
		}
		fields[key] = value
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if fields["username"] == "" && fields["password"] == "" {
		return nil, fmt.Errorf("Git credential provider returned no credential")
	}
	return fields, nil
}

func zeroCredentialBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
