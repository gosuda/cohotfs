//go:build linux

package proc

import (
	"fmt"
	"os"
	"syscall"
)

type SocketIdentity struct {
	Path  string `json:"path"`
	UID   int    `json:"uid"`
	Dev   uint64 `json:"dev"`
	Inode uint64 `json:"inode"`
	Mode  uint32 `json:"mode"`
}

func ReadSocket(path string, expectedUID int) (SocketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return SocketIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return SocketIdentity{}, fmt.Errorf("path is not a pathname Unix socket")
	}
	if info.Mode().Perm() != 0o600 {
		return SocketIdentity{}, fmt.Errorf("socket mode is %04o, want 0600", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return SocketIdentity{}, fmt.Errorf("socket identity unavailable")
	}
	if int(stat.Uid) != expectedUID {
		return SocketIdentity{}, fmt.Errorf("socket belongs to uid %d, want %d", stat.Uid, expectedUID)
	}
	return SocketIdentity{Path: path, UID: expectedUID, Dev: uint64(stat.Dev), Inode: stat.Ino, Mode: uint32(info.Mode().Perm())}, nil
}

func ValidateSocket(recorded SocketIdentity) error {
	current, err := ReadSocket(recorded.Path, recorded.UID)
	if err != nil {
		return err
	}
	if current.Dev != recorded.Dev || current.Inode != recorded.Inode || current.Mode != recorded.Mode {
		return fmt.Errorf("socket identity mismatch")
	}
	return nil
}
