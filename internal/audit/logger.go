// Package audit writes bounded events whose schema cannot contain payload data.
package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/hostroot"
	"golang.org/x/sys/unix"
)

const defaultMaxBytes int64 = 1 << 20

type Event struct {
	Time          time.Time `json:"time"`
	Operation     string    `json:"operation"`
	WorkspaceID   string    `json:"workspaceID,omitempty"`
	ResourceType  string    `json:"resourceType,omitempty"`
	Result        string    `json:"result"`
	ErrorCategory string    `json:"errorCategory,omitempty"`
}

type Logger struct {
	root     *hostroot.Root
	maxBytes int64
	now      func() time.Time
}

func New(root *hostroot.Root) *Logger {
	return &Logger{root: root, maxBytes: defaultMaxBytes, now: time.Now}
}

func (l *Logger) Append(event Event) error {
	if event.Operation == "" || event.Result == "" {
		return fmt.Errorf("audit operation and result are required")
	}
	if event.Time.IsZero() {
		event.Time = l.now().UTC()
	}
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	lockFile, err := l.root.OpenFile("run/audit.lock", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	file, err := l.root.OpenFile("logs/audit.jsonl", unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := validateLogFile(file, l.root.UID()); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(line)) > l.maxBytes {
		if err := file.Close(); err != nil {
			return err
		}
		if err := l.rotate(); err != nil {
			return err
		}
		file, err = l.root.OpenFile("logs/audit.jsonl", unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
		if err != nil {
			return err
		}
		if err := validateLogFile(file, l.root.UID()); err != nil {
			return err
		}
	}
	if _, err := file.Write(line); err != nil {
		return err
	}
	return file.Sync()
}

func validateLogFile(file *os.File, uid int) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("audit log type or mode is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid {
		return fmt.Errorf("audit log ownership is unsafe")
	}
	return nil
}

func (l *Logger) rotate() error {
	rotated, _ := l.root.HostPath("logs/audit.1.jsonl")
	if info, err := os.Lstat(rotated); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("rotated audit path is unsafe")
		}
		if err := l.root.Remove("logs/audit.1.jsonl"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := l.root.Rename("logs/audit.jsonl", "logs/audit.1.jsonl"); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}
