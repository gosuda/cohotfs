//go:build linux

package hostservice

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/bridgeproto"
	"github.com/gosuda/cohotfs/internal/proc"
)

func (s *Server) acquireWindowsChrome(ctx context.Context, request api.LeaseRequest) (Lease, api.LeaseResponse, error) {
	if os.Getenv("WSL_INTEROP") == "" || os.Getenv("WSL_DISTRO_NAME") == "" {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("Windows Chrome requires WSL interop")
	}
	executable := request.Parameters["executable"]
	if executable == "" || executable == "auto" {
		executable = s.hostConfig.Browser.WindowsExecutable
	}
	if executable == "" {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("Windows Chrome executable is not configured")
	}
	bridgeExecutable, err := s.root.HostPath(filepath.Join("bin", "cohotfs-windows-bridge.exe"))
	if err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	if info, statErr := os.Stat(bridgeExecutable); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("Windows Chrome companion is unavailable under the Cohotfs bin directory")
	}
	profileRootRel := "browser"
	profileRel := filepath.Join(profileRootRel, request.WorkspaceID)
	if err := s.root.EnsureDir(profileRel, 0o700); err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	profileRoot, _ := s.root.HostPath(profileRootRel)
	profile, _ := s.root.HostPath(profileRel)
	profileRootOutput, err := exec.CommandContext(ctx, "wslpath", "-w", profileRoot).Output()
	if err != nil {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("translate Windows Chrome profile root: %w", err)
	}
	profileOutput, err := exec.CommandContext(ctx, "wslpath", "-w", profile).Output()
	if err != nil {
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("translate Windows Chrome profile: %w", err)
	}
	command := exec.Command(bridgeExecutable, "serve",
		"--executable", executable,
		"--profile-root", stringTrimSpace(profileRootOutput),
		"--profile", stringTrimSpace(profileOutput),
		"--distro", os.Getenv("WSL_DISTRO_NAME"),
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Lease{}, api.LeaseResponse{}, err
	}
	command.Stderr = io.Discard
	command.Env = proc.SanitizedEnvironment(os.Environ())
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
	reader := bufio.NewReader(stdout)
	readyLine := make(chan []byte, 1)
	readyError := make(chan error, 1)
	go func() {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			readyError <- readErr
			return
		}
		readyLine <- line
	}()
	select {
	case <-ctx.Done():
		_ = terminateManagedProcess(context.Background(), managed)
		return Lease{}, api.LeaseResponse{}, ctx.Err()
	case err := <-readyError:
		_ = terminateManagedProcess(context.Background(), managed)
		return Lease{}, api.LeaseResponse{}, err
	case line := <-readyLine:
		var ready struct {
			Protocol string `json:"protocol"`
			Address  string `json:"ready"`
		}
		if json.Unmarshal(line, &ready) != nil || ready.Protocol != api.ProtocolVersion || ready.Address == "" {
			_ = terminateManagedProcess(context.Background(), managed)
			return Lease{}, api.LeaseResponse{}, fmt.Errorf("Windows Chrome companion readiness is invalid")
		}
	case <-time.After(20 * time.Second):
		_ = terminateManagedProcess(context.Background(), managed)
		return Lease{}, api.LeaseResponse{}, fmt.Errorf("Windows Chrome companion readiness timed out")
	}
	bridge, err := s.newFramedLeaseBridge(filepath.Join("run", "workspaces", request.WorkspaceID, "cdp.sock"), reader, stdin)
	if err != nil {
		_ = terminateManagedProcess(context.Background(), managed)
		return Lease{}, api.LeaseResponse{}, err
	}
	go func() { _ = command.Wait() }()
	closeLease := func(closeContext context.Context) error {
		bridgeErr := bridge.close(closeContext)
		processErr := terminateManagedProcess(closeContext, managed)
		var profileErr error
		if request.Parameters["retainProfile"] != "true" {
			profileErr = s.root.RemoveTree(profileRel)
		}
		return errors.Join(bridgeErr, processErr, profileErr)
	}
	lease := Lease{Summary: api.LeaseSummary{WorkspaceID: request.WorkspaceID, Kind: api.LeaseChrome}, Processes: []ManagedProcess{managed}, Sockets: []proc.SocketIdentity{bridge.identity}, Close: closeLease}
	return lease, api.LeaseResponse{Endpoint: "/run/cohotfs/host/cdp.sock", Metadata: socketLeaseMetadata(bridge.identity)}, nil
}

func (s *Server) newFramedLeaseBridge(relative string, input io.Reader, output io.WriteCloser) (leaseSocket, error) {
	listener, identity, err := s.listenLeaseSocket(relative)
	if err != nil {
		return leaseSocket{}, err
	}
	var nextID atomic.Uint32
	var outputMu sync.Mutex
	writeFrame := func(frame bridgeproto.Frame) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		return bridgeproto.Write(output, frame)
	}
	var connectionsMu sync.Mutex
	connections := map[uint32]net.Conn{}
	closeStream := func(id uint32) {
		connectionsMu.Lock()
		connection := connections[id]
		delete(connections, id)
		connectionsMu.Unlock()
		if connection != nil {
			_ = connection.Close()
		}
	}
	go func() {
		for {
			frame, readErr := bridgeproto.Read(input)
			if readErr != nil {
				connectionsMu.Lock()
				for _, connection := range connections {
					_ = connection.Close()
				}
				connectionsMu.Unlock()
				return
			}
			connectionsMu.Lock()
			connection := connections[frame.StreamID]
			connectionsMu.Unlock()
			if connection == nil {
				continue
			}
			switch frame.Kind {
			case bridgeproto.Data:
				if _, err := connection.Write(frame.Payload); err != nil {
					closeStream(frame.StreamID)
				}
			case bridgeproto.Close:
				closeStream(frame.StreamID)
			}
		}
	}()
	go func() {
		for {
			connection, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			id := nextID.Add(1)
			if id == 0 {
				_ = connection.Close()
				continue
			}
			connectionsMu.Lock()
			connections[id] = connection
			connectionsMu.Unlock()
			if err := writeFrame(bridgeproto.Frame{StreamID: id, Kind: bridgeproto.Open}); err != nil {
				closeStream(id)
				continue
			}
			go func(id uint32, connection net.Conn) {
				buffer := make([]byte, 32<<10)
				for {
					count, readErr := connection.Read(buffer)
					if count != 0 {
						if writeFrame(bridgeproto.Frame{StreamID: id, Kind: bridgeproto.Data, Payload: buffer[:count]}) != nil {
							closeStream(id)
							return
						}
					}
					if readErr != nil {
						closeStream(id)
						_ = writeFrame(bridgeproto.Frame{StreamID: id, Kind: bridgeproto.Close})
						return
					}
				}
			}(id, connection)
		}
	}()
	var once sync.Once
	closeSocket := func(context.Context) error {
		var closeErr error
		once.Do(func() {
			closeErr = listener.Close()
			connectionsMu.Lock()
			for _, connection := range connections {
				_ = connection.Close()
			}
			connections = map[uint32]net.Conn{}
			connectionsMu.Unlock()
			closeErr = errors.Join(closeErr, output.Close(), removeLeaseSocket(identity))
		})
		return closeErr
	}
	return leaseSocket{identity: identity, close: closeSocket}, nil
}

func stringTrimSpace(value []byte) string {
	for len(value) != 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	return string(value)
}
