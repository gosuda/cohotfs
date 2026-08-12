//go:build linux

package containeragent

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
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
		if output, err := exec.Command("/usr/sbin/useradd", "--uid", strconv.Itoa(bootstrap.OwnerUID), "--gid", strconv.Itoa(bootstrap.OwnerGID), "--home-dir", "/home/agent", "--create-home", "--shell", "/bin/sh", "agent").CombinedOutput(); err != nil {
			return fmt.Errorf("create agent user: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	output, err := exec.Command("/usr/sbin/usermod", "--password", "*", "agent").CombinedOutput()
	if err != nil {
		return fmt.Errorf("disable agent password: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := os.MkdirAll("/home/agent", 0o700); err != nil {
		return err
	}
	if err := os.Chown("/home/agent", bootstrap.OwnerUID, bootstrap.OwnerGID); err != nil {
		return err
	}
	return os.Chmod("/home/agent", 0o700)
}

func validateIdentity(passwd []passwdEntry, groups []groupEntry, uid, gid int) (userExists, groupExists bool, err error) {
	for _, entry := range passwd {
		if entry.Name == "agent" {
			if entry.UID != uid || entry.GID != gid || entry.Home != "/home/agent" || entry.Shell != "/bin/sh" {
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
