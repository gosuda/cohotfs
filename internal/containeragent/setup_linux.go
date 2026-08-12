//go:build linux

package containeragent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const setupOutputLimit = 1 << 20

type SetupResult struct {
	ExitCode  int    `json:"exitCode"`
	Output    []byte `json:"output"`
	Truncated bool   `json:"truncated"`
	TimedOut  bool   `json:"timedOut"`
}

func RunSetup(ctx context.Context, timeout time.Duration, argv []string) (SetupResult, error) {
	if timeout <= 0 || len(argv) == 0 || argv[0] == "" {
		return SetupResult{}, fmt.Errorf("setup requires positive timeout and argv")
	}
	setupContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = "/workspace"
	command.Env = []string{
		"HOME=/home/agent", "XDG_CONFIG_HOME=/home/agent/.config", "XDG_CACHE_HOME=/home/agent/.cache",
		"XDG_DATA_HOME=/home/agent/.local/share", "TMPDIR=/home/agent/.tmp", "PATH=/usr/local/bin:/usr/bin:/bin",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	capture := &boundedBuffer{limit: setupOutputLimit}
	command.Stdout, command.Stderr = capture, capture
	if err := command.Start(); err != nil {
		return SetupResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		result := SetupResult{ExitCode: exitCode(err), Output: append([]byte(nil), capture.Bytes()...), Truncated: capture.truncated}
		if err != nil {
			return result, fmt.Errorf("setup exited %d", result.ExitCode)
		}
		return result, nil
	case <-setupContext.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		grace := time.NewTimer(10 * time.Second)
		defer grace.Stop()
		select {
		case err := <-done:
			return SetupResult{ExitCode: exitCode(err), Output: append([]byte(nil), capture.Bytes()...), Truncated: capture.truncated, TimedOut: true}, setupContext.Err()
		case <-grace.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			err := <-done
			return SetupResult{ExitCode: exitCode(err), Output: append([]byte(nil), capture.Bytes()...), Truncated: capture.truncated, TimedOut: true}, setupContext.Err()
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.Len() >= b.limit {
		b.truncated = true
		return len(data), nil
	}
	remaining := b.limit - b.Len()
	write := data
	if len(write) > remaining {
		write = write[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(write)
	return len(data), nil
}

var _ = os.ErrProcessDone
