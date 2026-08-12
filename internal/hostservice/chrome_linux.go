//go:build linux

package hostservice

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/proc"
)

func (s *Server) acquireChrome(ctx context.Context, request api.LeaseRequest) (Lease, api.LeaseResponse, error) {
	platform := request.Parameters["platform"]
	if platform == "windows-wsl" {
		return s.acquireWindowsChrome(ctx, request)
	}
	if platform != "" && platform != "auto" && platform != "linux" {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("unsupported browser platform %s", platform)
	}
	executable := request.Parameters["executable"]
	if executable == "" || executable == "auto" {
		executable = s.hostConfig.Browser.LinuxExecutable
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("Chrome executable is unsafe")
	}
	profileRel := filepath.Join("browser", request.WorkspaceID)
	if err := s.root.EnsureDir(profileRel, 0o700); err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	profile, _ := s.root.HostPath(profileRel)
	command := exec.Command(canonical,
		"--user-data-dir="+profile,
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=0",
		"--no-first-run",
		"--no-default-browser-check",
	)
	command.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/local/bin:/usr/bin:/bin", "DISPLAY=" + os.Getenv("DISPLAY"), "WAYLAND_DISPLAY=" + os.Getenv("WAYLAND_DISPLAY"), "XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR")}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	identity, err := proc.ReadIdentity(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		return Lease{}, api.LeaseResponse{}, err
	}
	managed := ManagedProcess{Identity: identity, ProcessGroup: command.Process.Pid}
	go func() { _ = command.Wait() }()
	address, browserID, err := waitChromeReady(ctx, profile, command)
	if err != nil {
		_ = terminateManagedProcess(context.Background(), managed)
		return Lease{}, api.LeaseResponse{}, err
	}
	bridge, err := s.newLeaseTCPBridge(filepath.Join("run", "workspaces", request.WorkspaceID, "cdp.sock"), address)
	if err != nil {
		_ = terminateManagedProcess(context.Background(), managed)
		_ = s.root.RemoveTree(profileRel)
		return Lease{}, api.LeaseResponse{}, err
	}
	closeLease := func(closeContext context.Context) error {
		bridgeErr := bridge.close(closeContext)
		processErr := terminateManagedProcess(closeContext, managed)
		var profileErr error
		if request.Parameters["retainProfile"] != "true" {
			profileErr = s.root.RemoveTree(profileRel)
		}
		return errors.Join(bridgeErr, processErr, profileErr)
	}
	lease := Lease{
		Summary:   api.LeaseSummary{WorkspaceID: request.WorkspaceID, Kind: api.LeaseChrome},
		Processes: []ManagedProcess{managed},
		Sockets:   []proc.SocketIdentity{bridge.identity},
		Close:     closeLease,
	}
	metadata := socketLeaseMetadata(bridge.identity)
	metadata["browserID"] = browserID
	response := api.LeaseResponse{Endpoint: "/run/cohotfs/host/cdp.sock", Metadata: metadata}
	return lease, response, nil
}

func waitChromeReady(ctx context.Context, profile string, command *exec.Cmd) (string, string, error) {
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-deadline.C:
			return "", "", fmt.Errorf("Chrome DevTools readiness timed out")
		case <-ticker.C:
			if processExited(command.Process) {
				return "", "", fmt.Errorf("Chrome exited before DevTools readiness")
			}
			address, browserID, err := readDevToolsActivePort(filepath.Join(profile, "DevToolsActivePort"))
			if err != nil {
				continue
			}
			if err := verifyCDP(ctx, address); err == nil {
				return address, browserID, nil
			}
		}
	}
}

func readDevToolsActivePort(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return "", "", fmt.Errorf("missing DevTools port")
	}
	port, err := strconv.ParseUint(scanner.Text(), 10, 16)
	if err != nil || port == 0 {
		return "", "", fmt.Errorf("invalid DevTools port")
	}
	if !scanner.Scan() {
		return "", "", fmt.Errorf("missing DevTools browser ID")
	}
	browserID := strings.TrimSpace(scanner.Text())
	if !strings.HasPrefix(browserID, "/devtools/browser/") {
		return "", "", fmt.Errorf("invalid DevTools browser ID")
	}
	return "127.0.0.1:" + strconv.FormatUint(port, 10), browserID, nil
}

func verifyCDP(ctx context.Context, address string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/json/version", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("CDP returned HTTP %d", response.StatusCode)
	}
	var version map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil || len(version) == 0 {
		return fmt.Errorf("invalid CDP version response")
	}
	return nil
}

func processExited(process *os.Process) bool {
	if process == nil {
		return true
	}
	err := process.Signal(syscall.Signal(0))
	return err != nil
}
