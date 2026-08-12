package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gosuda/cohotfs/internal/containeragent"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
	"golang.org/x/crypto/ssh"
)

const (
	containerSSHHostPrivateKey = "/var/lib/cohotfs/system/ssh/ssh_host_ed25519_key"
	containerSSHHostPublicKey  = containerSSHHostPrivateKey + ".pub"
	containerAgentExecutable   = "/usr/local/libexec/cohotfs-agent"
)

func (s *DockerService) pinSSHHostKey(ctx context.Context, record *state.Workspace, expectedFingerprint string) error {
	output, err := s.waitForSSHHostPublicKey(ctx, record.RuntimeRef)
	if err != nil {
		return err
	}
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey(output)
	if err != nil || publicKey.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("workspace SSH host key is not a valid Ed25519 key")
	}
	if ssh.FingerprintSHA256(publicKey) != expectedFingerprint {
		return fmt.Errorf("workspace SSH host key does not match readiness response")
	}
	fingerprint := ssh.FingerprintSHA256(publicKey)
	if record.SSHHostFingerprint != "" && record.SSHHostFingerprint != fingerprint {
		return fmt.Errorf("workspace SSH host key changed")
	}
	if err := s.root.EnsureDir(filepath.Join("ssh", "known_hosts"), 0o700); err != nil {
		return err
	}
	relative := filepath.Join("ssh", "known_hosts", record.ID)
	line := "cohotfs-" + record.ID + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))) + "\n"
	if record.SSHHostFingerprint == "" {
		if err := s.root.AtomicWrite(relative, []byte(line), 0o600); err != nil {
			return err
		}
		record.SSHHostFingerprint = fingerprint
		path, _ := s.root.HostPath(relative)
		sum := sha256.Sum256([]byte(line))
		record.Resources = append(record.Resources, state.ExternalResource{
			Type: "ssh_known_hosts", ID: path, AcquiredAt: s.now().UTC(),
			Identity: map[string]string{"path": path, "sha256": hex.EncodeToString(sum[:])},
		})
		return s.store.SaveWorkspace(*record)
	}
	data, err := s.root.ReadFile(relative)
	if err != nil {
		return err
	}
	if string(data) != line {
		return fmt.Errorf("workspace known-host entry changed")
	}
	return nil
}

func (s *DockerService) waitForContainerReady(ctx context.Context, ref runtime.WorkspaceRef) (containeragent.Ready, error) {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		result, err := s.backend.ExecSync(ctx, ref, runtime.ExecRequest{
			Argv: []string{containerAgentExecutable, "ready"}, User: "0", Timeout: 2 * time.Second, OutputLimit: 16 << 10,
		})
		switch {
		case err != nil:
			lastErr = err
		case result.ExitCode != 0:
			lastErr = fmt.Errorf("exit code %d: %s", result.ExitCode, strings.TrimSpace(string(result.Output)))
		case result.Truncated:
			lastErr = fmt.Errorf("output was truncated")
		default:
			ready, decodeErr := containeragent.DecodeReady(bytes.NewReader(result.Output))
			if decodeErr == nil {
				return ready, nil
			}
			lastErr = decodeErr
		}
		select {
		case <-ctx.Done():
			return containeragent.Ready{}, ctx.Err()
		case <-deadline.C:
			return containeragent.Ready{}, fmt.Errorf("wait for workspace readiness: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func (s *DockerService) waitForSSHHostPublicKey(ctx context.Context, ref runtime.WorkspaceRef) ([]byte, error) {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		result, err := s.backend.ExecSync(ctx, ref, runtime.ExecRequest{
			Argv: []string{"/bin/cat", containerSSHHostPublicKey}, User: "0", Timeout: time.Second, OutputLimit: 16 << 10,
		})
		switch {
		case err != nil:
			lastErr = err
		case result.ExitCode != 0:
			lastErr = fmt.Errorf("exit code %d", result.ExitCode)
		case result.Truncated:
			lastErr = fmt.Errorf("output was truncated")
		default:
			return result.Output, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("read workspace SSH host key: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func (s *DockerService) removeSSHHostKey(record *state.Workspace) error {
	for index := range record.Resources {
		resource := &record.Resources[index]
		if resource.Type != "ssh_known_hosts" || resource.ReleasedAt != nil {
			continue
		}
		data, err := os.ReadFile(resource.Identity["path"])
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != resource.Identity["sha256"] {
				return fmt.Errorf("workspace known-host entry identity mismatch")
			}
			if err := s.root.Remove(filepath.Join("ssh", "known_hosts", record.ID)); err != nil {
				return err
			}
		}
		released := s.now().UTC()
		resource.ReleasedAt = &released
	}
	return nil
}
