//go:build linux

// Package proc performs identity-safe process and socket validation.
package proc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type Identity struct {
	PID              int    `json:"pid"`
	UID              int    `json:"uid"`
	StartTicks       uint64 `json:"startTicks"`
	Executable       string `json:"executable"`
	ExecutableDigest string `json:"executableDigest"`
}

func CurrentIdentity() (Identity, error) { return ReadIdentity(os.Getpid()) }

func ReadIdentity(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("invalid pid")
	}
	start, err := StartTicks(pid)
	if err != nil {
		return Identity{}, err
	}
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return Identity{}, fmt.Errorf("read executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return Identity{}, fmt.Errorf("canonicalize executable: %w", err)
	}
	digest, err := FileDigest(executable)
	if err != nil {
		return Identity{}, err
	}
	status, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return Identity{}, err
	}
	stat, ok := status.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, fmt.Errorf("process ownership unavailable")
	}
	return Identity{PID: pid, UID: int(stat.Uid), StartTicks: start, Executable: executable, ExecutableDigest: digest}, nil
}

func StartTicks(pid int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	closing := strings.LastIndexByte(string(raw), ')')
	if closing < 0 || closing+2 >= len(raw) {
		return 0, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(string(raw[closing+2:]))
	// The remainder begins at field 3; process start time is field 22.
	if len(fields) <= 19 {
		return 0, fmt.Errorf("malformed process stat fields")
	}
	value, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process start time: %w", err)
	}
	return value, nil
}

func FileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func Matches(recorded Identity) error {
	current, err := ReadIdentity(recorded.PID)
	if err != nil {
		return err
	}
	if current.UID != recorded.UID || current.StartTicks != recorded.StartTicks || current.Executable != recorded.Executable || current.ExecutableDigest != recorded.ExecutableDigest {
		return fmt.Errorf("process identity mismatch")
	}
	return nil
}

var hostEnvironmentAllowlist = map[string]bool{
	"PATH": true, "LANG": true, "LC_ALL": true, "TZ": true,
	"DISPLAY": true, "WAYLAND_DISPLAY": true, "XDG_RUNTIME_DIR": true,
	"WSL_DISTRO_NAME": true, "WSL_INTEROP": true, "WSLENV": true,
	"SYSTEMROOT": true, "WINDIR": true,
}

func SanitizedEnvironment(source []string) []string {
	result := make([]string, 0, len(source))
	for _, entry := range source {
		key, _, ok := strings.Cut(entry, "=")
		if ok && hostEnvironmentAllowlist[key] {
			result = append(result, entry)
		}
	}
	return result
}
