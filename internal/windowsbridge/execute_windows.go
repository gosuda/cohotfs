//go:build windows

package windowsbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gosuda/cohotfs/internal/bridgeproto"
	"golang.org/x/sys/windows"
)

var Version = "dev"

func Execute(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, Version)
		return nil
	}
	if len(args) < 1 || args[0] != "serve" {
		return fmt.Errorf("usage: cohotfs-windows-bridge <serve|version>")
	}
	options, err := parseServeArgs(args[1:])
	if err != nil {
		return err
	}
	modulePath, err := os.Executable()
	if err != nil {
		return err
	}
	if err := validateProfileBoundary(modulePath, options.profileRoot, options.profile, options.distro); err != nil {
		return err
	}
	return serveChrome(context.Background(), options.executable, options.profile, stdin, stdout)
}

type serveOptions struct {
	executable  string
	profile     string
	profileRoot string
	distro      string
}

func parseServeArgs(args []string) (serveOptions, error) {
	var options serveOptions
	for len(args) != 0 {
		if len(args) < 2 {
			return serveOptions{}, fmt.Errorf("serve option requires a value")
		}
		switch args[0] {
		case "--executable":
			options.executable = args[1]
		case "--profile":
			options.profile = args[1]
		case "--profile-root":
			options.profileRoot = args[1]
		case "--distro":
			options.distro = args[1]
		default:
			return serveOptions{}, fmt.Errorf("unknown serve option %s", args[0])
		}
		args = args[2:]
	}
	if !filepath.IsAbs(options.executable) || options.profile == "" || options.profileRoot == "" || options.distro == "" {
		return serveOptions{}, fmt.Errorf("serve requires absolute --executable and WSL --profile, --profile-root, and --distro")
	}
	return options, nil
}

func serveChrome(ctx context.Context, executable, profile string, stdin io.Reader, stdout io.Writer) error {
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	info, err := os.Stat(canonical)
	if err != nil || info.IsDir() {
		return fmt.Errorf("Chrome executable is invalid")
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	job, err := createKillJob()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	command := exec.Command(canonical, "--user-data-dir="+profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check")
	if err := command.Start(); err != nil {
		return err
	}
	if err := command.Process.WithHandle(func(handle uintptr) { err = windows.AssignProcessToJobObject(job, windows.Handle(handle)) }); err != nil {
		_ = command.Process.Kill()
		return err
	}
	if err != nil {
		_ = command.Process.Kill()
		return err
	}
	go func() { _ = command.Wait() }()
	address, err := waitDevToolsPort(ctx, profile)
	if err != nil {
		_ = windows.TerminateJobObject(job, 1)
		return err
	}
	if err := json.NewEncoder(stdout).Encode(map[string]string{"protocol": "v1alpha1", "ready": address}); err != nil {
		return err
	}
	return relayFrames(ctx, address, stdin, stdout)
}

func relayFrames(ctx context.Context, address string, input io.Reader, output io.Writer) error {
	connections := map[uint32]net.Conn{}
	var connectionsMu sync.Mutex
	var outputMu sync.Mutex
	writeFrame := func(frame bridgeproto.Frame) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		return bridgeproto.Write(output, frame)
	}
	closeStream := func(id uint32) {
		connectionsMu.Lock()
		connection := connections[id]
		delete(connections, id)
		connectionsMu.Unlock()
		if connection != nil {
			_ = connection.Close()
		}
	}
	defer func() {
		connectionsMu.Lock()
		defer connectionsMu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	for {
		frame, err := bridgeproto.Read(input)
		if err != nil {
			return err
		}
		switch frame.Kind {
		case bridgeproto.Open:
			connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", address)
			if err != nil {
				_ = writeFrame(bridgeproto.Frame{StreamID: frame.StreamID, Kind: bridgeproto.Close})
				continue
			}
			connectionsMu.Lock()
			if connections[frame.StreamID] != nil {
				connectionsMu.Unlock()
				_ = connection.Close()
				return fmt.Errorf("duplicate bridge stream ID")
			}
			connections[frame.StreamID] = connection
			connectionsMu.Unlock()
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
			}(frame.StreamID, connection)
		case bridgeproto.Data:
			connectionsMu.Lock()
			connection := connections[frame.StreamID]
			connectionsMu.Unlock()
			if connection == nil {
				return fmt.Errorf("data for unknown bridge stream")
			}
			if _, err := connection.Write(frame.Payload); err != nil {
				closeStream(frame.StreamID)
			}
		case bridgeproto.Close:
			closeStream(frame.StreamID)
		}
	}
}

func createKillJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func waitDevToolsPort(ctx context.Context, profile string) (string, error) {
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("Windows Chrome DevTools readiness timed out")
		case <-ticker.C:
			file, err := os.Open(filepath.Join(profile, "DevToolsActivePort"))
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(file)
			ok := scanner.Scan()
			port := strings.TrimSpace(scanner.Text())
			_ = file.Close()
			value, parseErr := strconv.ParseUint(port, 10, 16)
			if ok && parseErr == nil && value != 0 {
				return "127.0.0.1:" + port, nil
			}
		}
	}
}
