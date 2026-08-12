//go:build linux

package containeragent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultReadySocket = "/run/cohotfs/control/ready.sock"

type Ready struct {
	BootstrapAPI       string `json:"bootstrapAPI"`
	SSHAddress         string `json:"sshAddress"`
	SSHHostFingerprint string `json:"sshHostFingerprint"`
	SSHRelay           bool   `json:"sshRelay"`
	TCPForwarding      bool   `json:"tcpForwarding"`
}

func ValidateReady(ready Ready) error {
	if ready.BootstrapAPI != BootstrapAPI {
		return fmt.Errorf("readiness bootstrap API is %q, want %q", ready.BootstrapAPI, BootstrapAPI)
	}
	if ready.SSHAddress != "127.0.0.1:2222" {
		return fmt.Errorf("readiness SSH address is %q", ready.SSHAddress)
	}
	if !ready.SSHRelay || !ready.TCPForwarding {
		return fmt.Errorf("readiness transport capabilities are incomplete")
	}
	encoded, ok := strings.CutPrefix(ready.SSHHostFingerprint, "SHA256:")
	if !ok {
		return fmt.Errorf("readiness SSH host fingerprint is invalid")
	}
	digest, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("readiness SSH host fingerprint is invalid")
	}
	return nil
}

func DecodeReady(reader io.Reader) (Ready, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 16<<10))
	decoder.DisallowUnknownFields()
	var ready Ready
	if err := decoder.Decode(&ready); err != nil {
		return Ready{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Ready{}, fmt.Errorf("readiness response contains multiple values")
		}
		return Ready{}, err
	}
	if err := ValidateReady(ready); err != nil {
		return Ready{}, err
	}
	return ready, nil
}

func ProbeReadiness(ctx context.Context, path string) (Ready, error) {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return Ready{}, err
	}
	defer connection.Close()
	deadline := time.Now().Add(time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return Ready{}, err
	}
	return DecodeReady(connection)
}
func Serve(ctx context.Context, bootstrapPath string) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("cohotfs-agent serve must run as PID 1")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("cohotfs-agent serve must start as uid 0")
	}
	if _, err := CheckImage("/", BootstrapAPI); err != nil {
		return err
	}
	bootstrap, err := LoadBootstrap(bootstrapPath)
	if err != nil {
		return err
	}
	if err := EnsureAgentIdentity(bootstrap); err != nil {
		return err
	}
	if err := installConfiguredIntegrations(bootstrap); err != nil {
		return err
	}
	fingerprint, err := EnsureHostKey(defaultHostKeyPath)
	if err != nil {
		return err
	}
	if err := InstallAuthorizedKey(bootstrap.AuthorizedKeyPath, defaultAuthorizedKeys); err != nil {
		return err
	}
	if err := WriteAndValidateSSHDConfig(defaultSSHDConfig, bootstrap); err != nil {
		return err
	}

	sshd := exec.Command("/usr/sbin/sshd", "-D", "-e", "-f", defaultSSHDConfig)
	sshd.Stdout = os.Stderr
	sshd.Stderr = os.Stderr
	if err := sshd.Start(); err != nil {
		return err
	}
	childPID := sshd.Process.Pid
	childEvents := make(chan os.Signal, 1)
	signal.Notify(childEvents, syscall.SIGCHLD)
	defer signal.Stop(childEvents)

	helperCtx, cancelHelpers := context.WithCancel(ctx)
	defer cancelHelpers()
	helperErrors := make(chan error, 3)
	go func() {
		helperErrors <- RunSSHRelay(helperCtx, defaultSSHRelaySocket, "127.0.0.1:2222", bootstrap.OwnerUID, bootstrap.OwnerGID)
	}()
	if err := waitForUnixSocket(ctx, defaultSSHRelaySocket, 10*time.Second); err != nil {
		_ = sshd.Process.Kill()
		return err
	}
	if bootstrap.EnableCDP {
		go func() { helperErrors <- RunCDPProxy(helperCtx, "127.0.0.1:9222", defaultCDPSocket) }()
	}
	if err := waitForTCP(ctx, "127.0.0.1:2222", 10*time.Second); err != nil {
		_ = sshd.Process.Kill()
		return err
	}
	ready := Ready{BootstrapAPI: BootstrapAPI, SSHAddress: "127.0.0.1:2222", SSHHostFingerprint: fingerprint, SSHRelay: true, TCPForwarding: true}
	go func() { helperErrors <- serveReadiness(helperCtx, defaultReadySocket, ready) }()

	for {
		select {
		case <-ctx.Done():
			cancelHelpers()
			return terminateChild(childPID, childEvents)
		case err := <-helperErrors:
			if err != nil {
				_ = unix.Kill(childPID, unix.SIGTERM)
				return fmt.Errorf("mandatory socket helper failed: %w", err)
			}
		case <-childEvents:
			dead, status, err := reapChildren(childPID)
			if err != nil {
				return err
			}
			if dead {
				return fmt.Errorf("sshd exited with status %d", status)
			}
		}
	}
}

func reapChildren(mandatoryPID int) (dead bool, exitCode int, err error) {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) || pid == 0 {
			return false, 0, nil
		}
		if err != nil {
			return false, 0, err
		}
		if pid == mandatoryPID {
			if status.Exited() {
				return true, status.ExitStatus(), nil
			}
			if status.Signaled() {
				return true, 128 + int(status.Signal()), nil
			}
			return true, 1, nil
		}
	}
}

func terminateChild(pid int, childEvents <-chan os.Signal) error {
	if err := unix.Kill(pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-childEvents:
			dead, _, err := reapChildren(pid)
			if err != nil {
				return err
			}
			if dead {
				return nil
			}
		case <-timer.C:
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return err
			}
			var status unix.WaitStatus
			_, err := unix.Wait4(pid, &status, 0, nil)
			if errors.Is(err, unix.ECHILD) {
				return nil
			}
			return err
		}
	}
}

func waitForTCP(ctx context.Context, address string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("tcp4", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("sshd readiness timed out")
		case <-ticker.C:
		}
	}
}

func waitForUnixSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("SSH relay readiness timed out")
		case <-ticker.C:
		}
	}
}

func clearStaleReadinessSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("readiness path exists and is not a Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("readiness socket ownership is unsafe")
	}
	return os.Remove(path)
}

func serveReadiness(ctx context.Context, path string, ready Ready) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := clearStaleReadinessSocket(path); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		_ = json.NewEncoder(connection).Encode(ready)
		_ = connection.Close()
	}
}
