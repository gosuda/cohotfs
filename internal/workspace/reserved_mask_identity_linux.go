//go:build linux

package workspace

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func reservedWorkspaceMaskIdentity(name string, info os.FileInfo) (ReservedWorkspaceMask, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ReservedWorkspaceMask{}, fmt.Errorf("reserved workspace path %s identity is unavailable", name)
	}
	return ReservedWorkspaceMask{
		Name: name, Device: uint64(stat.Dev), Inode: stat.Ino,
		Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid,
	}, nil
}

func openReservedWorkspaceMask(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
