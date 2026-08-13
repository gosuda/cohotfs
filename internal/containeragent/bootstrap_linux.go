//go:build linux

package containeragent

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const defaultBootstrapPath = "/run/cohotfs/bootstrap/bootstrap.json"

var workspaceIDPattern = regexp.MustCompile(`^[a-z2-7]{26}$`)

type Bootstrap struct {
	BootstrapAPI         string `json:"bootstrapAPI"`
	OwnerUID             int    `json:"ownerUID"`
	OwnerGID             int    `json:"ownerGID"`
	WorkspaceID          string `json:"workspaceID"`
	AuthorizedKeyPath    string `json:"authorizedKeyPath"`
	AllowAgentForwarding bool   `json:"allowAgentForwarding"`
	EnableCDP            bool   `json:"enableCDP"`
	EnableGitCredentials bool   `json:"enableGitCredentials"`
	EnableAgentSecrets   bool   `json:"enableAgentSecrets"`
}

func LoadBootstrap(path string) (Bootstrap, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Bootstrap{}, err
	}
	return ParseBootstrap(raw)
}

func ParseBootstrap(raw []byte) (Bootstrap, error) {
	var bootstrap Bootstrap
	if err := decodeOneJSON(bytes.NewReader(raw), &bootstrap); err != nil {
		return Bootstrap{}, err
	}
	if bootstrap.BootstrapAPI != BootstrapAPI || !workspaceIDPattern.MatchString(bootstrap.WorkspaceID) || bootstrap.OwnerUID <= 0 || bootstrap.OwnerGID <= 0 || !filepath.IsAbs(bootstrap.AuthorizedKeyPath) {
		return Bootstrap{}, fmt.Errorf("invalid bootstrap contract")
	}
	return bootstrap, nil
}

type passwdEntry struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
}

type groupEntry struct {
	Name string
	GID  int
}

func EnsureAgentIdentity(bootstrap Bootstrap) error {
	passwd, err := readPasswd("/etc/passwd")
	if err != nil {
		return err
	}
	groups, err := readGroups("/etc/group")
	if err != nil {
		return err
	}
	userExists, groupExists, err := validateIdentity(passwd, groups, bootstrap.OwnerUID, bootstrap.OwnerGID)
	if err != nil {
		return fmt.Errorf("image_identity_conflict: %w", err)
	}
	if !groupExists {
		if output, err := exec.Command("/usr/sbin/groupadd", "--gid", strconv.Itoa(bootstrap.OwnerGID), "agent").CombinedOutput(); err != nil {
			return fmt.Errorf("create agent group: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if !userExists {
		if output, err := exec.Command("/usr/sbin/useradd", "--uid", strconv.Itoa(bootstrap.OwnerUID), "--gid", strconv.Itoa(bootstrap.OwnerGID), "--home-dir", "/home/agent", "--create-home", "--shell", "/bin/bash", "agent").CombinedOutput(); err != nil {
			return fmt.Errorf("create agent user: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	output, err := exec.Command("/usr/sbin/usermod", "--password", "*", "agent").CombinedOutput()
	if err != nil {
		return fmt.Errorf("disable agent password: %w: %s", err, strings.TrimSpace(string(output)))
	}
	homeFD, err := ensureAgentHome("/home", "agent", bootstrap.OwnerUID, bootstrap.OwnerGID)
	if err != nil {
		return err
	}
	defer unix.Close(homeFD)
	return installShellProfileAt(homeFD, bootstrap.OwnerUID, bootstrap.OwnerGID)
}

func ensureAgentHome(parent, name string, uid, gid int) (int, error) {
	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(parentFD)
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("agent home is not a directory")
	}
	if err := unix.Fchown(fd, uid, gid); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func installShellProfile(home string, uid, gid int) error {
	homeFD, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(homeFD)
	return installShellProfileAt(homeFD, uid, gid)
}

func installShellProfileAt(homeFD, uid, gid int) error {
	managed := "# Managed by Cohotfs.\n" + managedShellExports(os.Environ()) + `
if [[ $- == *i* ]]; then
  export TERM="${TERM:-xterm-256color}"
  export COLORTERM="${COLORTERM:-truecolor}"
  export CLICOLOR=1
  alias ls='ls --color=auto'
  alias grep='grep --color=auto'
  PS1='\[\033[1;36m\]\u@cohotfs\[\033[0m\]:\[\033[1;34m\]\w\[\033[0m\]\$ '
fi
`
	configFD, err := ensureShellDirectory(homeFD, ".config", uid, gid)
	if err != nil {
		return err
	}
	defer unix.Close(configFD)
	cohotfsFD, err := ensureShellDirectory(configFD, "cohotfs", uid, gid)
	if err != nil {
		return err
	}
	defer unix.Close(cohotfsFD)
	if err := writeOwnedShellFile(cohotfsFD, "bashrc", managed, uid, gid); err != nil {
		return err
	}
	if err := appendOwnedShellBlock(homeFD, ".bashrc", "# Cohotfs interactive defaults\nif [ -r \"$HOME/.config/cohotfs/bashrc\" ]; then . \"$HOME/.config/cohotfs/bashrc\"; fi\n", uid, gid); err != nil {
		return err
	}
	return appendOwnedShellBlock(homeFD, ".bash_profile", "# Cohotfs login shell\nif [ -r \"$HOME/.bashrc\" ]; then . \"$HOME/.bashrc\"; fi\nif [ -r \"$HOME/.config/cohotfs/bashrc\" ]; then . \"$HOME/.config/cohotfs/bashrc\"; fi\n", uid, gid)
}

var managedShellEnvironmentNames = []string{
	"PATH", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"PI_CODING_AGENT_DIR", "COHOTFS_CDP_URL", "COHOTFS_MANAGED_TOOLCHAINS",
	"GOROOT", "GOTOOLCHAIN", "GOMODCACHE", "GOCACHE", "GOPATH", "GOBIN", "GOENV", "GOTMPDIR",
	"RUSTC", "RUSTDOC", "CARGO_HOME", "CARGO_TARGET_DIR", "CARGO_INSTALL_ROOT",
}

func managedShellExports(environment []string) string {
	allowed := make(map[string]bool, len(managedShellEnvironmentNames))
	for _, name := range managedShellEnvironmentNames {
		allowed[name] = true
	}
	values := make(map[string]string, len(managedShellEnvironmentNames))
	for _, item := range environment {
		name, value, ok := strings.Cut(item, "=")
		if ok && allowed[name] {
			values[name] = value
		}
	}
	var result strings.Builder
	for _, name := range managedShellEnvironmentNames {
		if value, ok := values[name]; ok {
			fmt.Fprintf(&result, "export %s='%s'\n", name, strings.ReplaceAll(value, "'", "'\"'\"'"))
		}
	}
	return result.String()
}

func ensureShellDirectory(parentFD int, name string, uid, gid int) (int, error) {
	created := false
	if err := unix.Mkdirat(parentFD, name, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if created {
		if err := unix.Fchown(fd, uid, gid); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	if err := validateShellPath(fd, uid, gid, true); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func writeOwnedShellFile(parentFD int, name, content string, uid, gid int) error {
	file, err := openOwnedShellFile(parentFD, name, uid, gid)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		return err
	}
	return file.Sync()
}

func appendOwnedShellBlock(parentFD int, name, block string, uid, gid int) error {
	file, err := openOwnedShellFile(parentFD, name, uid, gid)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 1<<20 {
		return fmt.Errorf("%s exceeds 1 MiB", name)
	}
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return err
	}
	marker := strings.SplitN(block, "\n", 2)[0]
	if strings.Contains(string(raw), marker) {
		return nil
	}
	if len(raw) != 0 && raw[len(raw)-1] != '\n' {
		block = "\n" + block
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := io.WriteString(file, block); err != nil {
		return err
	}
	return file.Sync()
}

func openOwnedShellFile(parentFD int, name string, uid, gid int) (*os.File, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err == nil {
		if chownErr := unix.Fchown(fd, uid, gid); chownErr != nil {
			_ = unix.Close(fd)
			return nil, chownErr
		}
	} else if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(parentFD, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := validateShellPath(fd, uid, gid, false); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func validateShellPath(fd, uid, gid int, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	expected := uint32(unix.S_IFREG)
	if directory {
		expected = unix.S_IFDIR
	}
	if stat.Mode&unix.S_IFMT != expected || int(stat.Uid) != uid || int(stat.Gid) != gid || stat.Mode&0o022 != 0 {
		return fmt.Errorf("unsafe shell profile path")
	}
	return nil
}

func validateIdentity(passwd []passwdEntry, groups []groupEntry, uid, gid int) (userExists, groupExists bool, err error) {
	for _, entry := range passwd {
		if entry.Name == "agent" {
			if entry.UID != uid || entry.GID != gid || entry.Home != "/home/agent" || entry.Shell != "/bin/bash" {
				return false, false, fmt.Errorf("existing agent user does not match requested identity")
			}
			userExists = true
		} else if entry.UID == uid {
			return false, false, fmt.Errorf("uid %d belongs to user %s", uid, entry.Name)
		}
	}
	for _, entry := range groups {
		if entry.Name == "agent" {
			if entry.GID != gid {
				return false, false, fmt.Errorf("existing agent group does not match requested gid")
			}
			groupExists = true
		} else if entry.GID == gid {
			return false, false, fmt.Errorf("gid %d belongs to group %s", gid, entry.Name)
		}
	}
	return userExists, groupExists, nil
}

func readPasswd(path string) ([]passwdEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []passwdEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) != 7 {
			return nil, fmt.Errorf("malformed passwd entry")
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		gid, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, err
		}
		entries = append(entries, passwdEntry{Name: fields[0], UID: uid, GID: gid, Home: fields[5], Shell: fields[6]})
	}
	return entries, scanner.Err()
}

func readGroups(path string) ([]groupEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []groupEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) != 4 {
			return nil, fmt.Errorf("malformed group entry")
		}
		gid, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		entries = append(entries, groupEntry{Name: fields[0], GID: gid})
	}
	return entries, scanner.Err()
}

func InstallAuthorizedKey(source, destination string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(raw))
	if strings.ContainsAny(key, "\r\n") {
		return fmt.Errorf("authorized public key must contain one line")
	}
	fields := strings.Fields(key)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return fmt.Errorf("authorized public key must be ssh-ed25519 without options")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 32 || len(blob) > 1024 {
		return fmt.Errorf("authorized public key encoding is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(destination, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), destination)
	if os.Geteuid() == 0 {
		if err := unix.Fchown(fd, 0, 0); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := unix.Fchmod(fd, 0o644); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.WriteString(key + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
