//go:build linux

// Package hostroot owns the canonical per-user Cohotfs filesystem boundary.
package hostroot

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const dirname = ".cohotfs"

var layout = []string{
	"bin",
	"projects",
	"state",
	"state/workspaces",
	"state/images",
	"workspaces",
	"ssh",
	"browser",
	"cache",
	"logs",
	"run",
	"run/workspaces",
	"tmp",
}

// Root is an open, identity-checked handle to ~/.cohotfs.
type Root struct {
	path string
	fd   int
	uid  int
}

// Open resolves the current UID through the operating-system user database. It
// deliberately ignores HOME so production state cannot be relocated.
func Open() (*Root, error) {
	uid := os.Getuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return nil, fmt.Errorf("resolve home for uid %d: %w", uid, err)
	}
	if !filepath.IsAbs(u.HomeDir) {
		return nil, fmt.Errorf("home for uid %d is not absolute", uid)
	}
	return openAt(filepath.Join(u.HomeDir, dirname), uid)
}

// OpenForTest constructs the same boundary at an injected path. It is internal
// to the module and is never wired to a CLI flag or environment variable.
func OpenForTest(path string) (*Root, error) { return openAt(path, os.Getuid()) }

func openAt(path string, uid int) (*Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("host root must be absolute")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, fmt.Errorf("create host root: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect host root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("host root is a symlink")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("host root is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("host root ownership unavailable")
	}
	if int(stat.Uid) != uid {
		return nil, fmt.Errorf("host root belongs to uid %d, want %d", stat.Uid, uid)
	}
	if info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("host root mode is %04o, want 0700", info.Mode().Perm())
	}

	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open host root: %w", err)
	}
	r := &Root{path: path, fd: fd, uid: uid}
	for _, rel := range layout {
		if err := r.EnsureDir(rel, 0o700); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}
	return r, nil
}

func (r *Root) Path() string { return r.path }
func (r *Root) UID() int     { return r.uid }
func (r *Root) Close() error { return unix.Close(r.fd) }

func cleanRelative(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, '\x00') {
		return "", fmt.Errorf("path must be non-empty and relative")
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != rel {
		return "", fmt.Errorf("unclean or escaping path %q", rel)
	}
	return clean, nil
}

func openBeneath(dirfd int, rel string, flags int, mode uint32) (int, error) {
	return unix.Openat2(dirfd, rel, &unix.OpenHow{
		Flags: uint64(flags | unix.O_CLOEXEC),
		Mode:  uint64(mode),
		Resolve: unix.RESOLVE_BENEATH |
			unix.RESOLVE_NO_MAGICLINKS |
			unix.RESOLVE_NO_SYMLINKS,
	})
}

func (r *Root) EnsureDir(rel string, mode os.FileMode) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	parts := strings.Split(clean, string(filepath.Separator))
	fd := r.fd
	ownedFD := -1
	defer func() {
		if ownedFD >= 0 {
			_ = unix.Close(ownedFD)
		}
	}()
	for _, part := range parts {
		next, openErr := openBeneath(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, part, uint32(mode.Perm())); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return fmt.Errorf("create %s: %w", clean, mkdirErr)
			}
			next, openErr = openBeneath(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			return fmt.Errorf("open %s: %w", clean, openErr)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(next, &stat); err != nil {
			_ = unix.Close(next)
			return fmt.Errorf("stat %s: %w", clean, err)
		}
		if int(stat.Uid) != r.uid {
			_ = unix.Close(next)
			return fmt.Errorf("%s belongs to uid %d, want %d", clean, stat.Uid, r.uid)
		}
		if os.FileMode(stat.Mode).Perm() != mode.Perm() {
			_ = unix.Close(next)
			return fmt.Errorf("%s mode is %04o, want %04o", clean, os.FileMode(stat.Mode).Perm(), mode.Perm())
		}
		if ownedFD >= 0 {
			_ = unix.Close(ownedFD)
		}
		ownedFD = next
		fd = next
	}
	return nil
}

func (r *Root) OpenFile(rel string, flags int, mode os.FileMode) (*os.File, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return nil, err
	}
	fd, err := openBeneath(r.fd, clean, flags|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", clean, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(r.path, clean)), nil
}

func (r *Root) ReadFile(rel string) ([]byte, error) {
	f, err := r.OpenFile(rel, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// AtomicWrite persists a regular file by fsync, rename, and parent fsync.
func (r *Root) AtomicWrite(rel string, data []byte, mode os.FileMode) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	parent, name := filepath.Split(clean)
	parent = strings.TrimSuffix(parent, string(filepath.Separator))
	if parent == "" {
		parent = "."
	}
	parentFD, err := openBeneath(r.fd, parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open parent %s: %w", parent, err)
	}
	defer unix.Close(parentFD)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("random temporary name: %w", err)
	}
	tmp := fmt.Sprintf(".%s.tmp-%x", name, suffix[:])
	fd, err := openBeneath(parentFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(r.path, parent, tmp))
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = unix.Unlinkat(parentFD, tmp, 0)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", clean, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", clean, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", clean, err)
	}
	if err := unix.Renameat(parentFD, tmp, parentFD, name); err != nil {
		return fmt.Errorf("rename %s: %w", clean, err)
	}
	cleanup = false
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("sync parent %s: %w", parent, err)
	}
	return nil
}

func (r *Root) openParent(rel string) (int, string, error) {
	parent, name := filepath.Split(rel)
	parent = strings.TrimSuffix(parent, string(filepath.Separator))
	if name == "" {
		return -1, "", fmt.Errorf("path has no leaf")
	}
	if parent == "" {
		parent = "."
	}
	if parent == "." {
		fd, err := unix.Openat(r.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		return fd, name, err
	}
	fd, err := openBeneath(r.fd, parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	return fd, name, err
}

func (r *Root) Remove(rel string) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	parentFD, name, err := r.openParent(clean)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
}

func (r *Root) Rename(oldRel, newRel string) error {
	oldClean, err := cleanRelative(oldRel)
	if err != nil {
		return err
	}
	newClean, err := cleanRelative(newRel)
	if err != nil {
		return err
	}
	oldParent, oldName, err := r.openParent(oldClean)
	if err != nil {
		return err
	}
	defer unix.Close(oldParent)
	newParent, newName, err := r.openParent(newClean)
	if err != nil {
		return err
	}
	defer unix.Close(newParent)
	if err := unix.Renameat(oldParent, oldName, newParent, newName); err != nil {
		return err
	}
	if err := unix.Fsync(newParent); err != nil {
		return err
	}
	if oldParent != newParent {
		return unix.Fsync(oldParent)
	}
	return nil
}

func (r *Root) RemoveTree(rel string) error {
	clean, err := cleanRelative(rel)
	if err != nil {
		return err
	}
	parentFD, name, err := r.openParent(clean)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	fd, err := openBeneath(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := removeTreeFD(fd); err != nil {
		unix.Close(fd)
		return err
	}
	if err := unix.Close(fd); err != nil {
		return err
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
}

func removeTreeFD(fd int) error {
	readFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(readFD), "")
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		if entry.Name() == "." || entry.Name() == ".." {
			return fmt.Errorf("invalid directory entry")
		}
		if entry.IsDir() {
			child, err := openBeneath(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			if err := removeTreeFD(child); err != nil {
				unix.Close(child)
				return err
			}
			if err := unix.Close(child); err != nil {
				return err
			}
			if err := unix.Unlinkat(fd, entry.Name(), unix.AT_REMOVEDIR); err != nil {
				return err
			}
			continue
		}
		if err := unix.Unlinkat(fd, entry.Name(), 0); err != nil {
			return err
		}
	}
	return unix.Fsync(fd)
}

func (r *Root) HostPath(rel string) (string, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.path, clean), nil
}

func (r *Root) SocketPath(rel string) (string, error) {
	path, err := r.HostPath(rel)
	if err != nil {
		return "", err
	}
	if len(path) >= len(unix.RawSockaddrUnix{}.Path) {
		return "", fmt.Errorf("unix socket path is %d bytes; maximum is %d", len(path), len(unix.RawSockaddrUnix{}.Path)-1)
	}
	return path, nil
}
