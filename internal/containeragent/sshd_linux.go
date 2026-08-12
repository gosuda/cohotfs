//go:build linux

package containeragent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
)

const (
	defaultSystemDir                      = "/var/lib/cohotfs/system"
	defaultHostKeyPath                    = defaultSystemDir + "/ssh/ssh_host_ed25519_key"
	defaultAuthorizedKeys                 = defaultSystemDir + "/authorized_keys"
	defaultSSHDConfig                     = "/run/cohotfs/sshd_config"
	activeSystemDirectoryMode os.FileMode = 0o711
)

func secureSystemStorage(keyPath string) error {
	sshDirectory := filepath.Dir(keyPath)
	systemDirectory := filepath.Dir(sshDirectory)
	info, err := os.Lstat(systemDirectory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("system storage type is unsafe")
	}
	if err := os.Chown(systemDirectory, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(systemDirectory, activeSystemDirectoryMode); err != nil {
		return err
	}
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		return err
	}
	info, err = os.Lstat(sshDirectory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("SSH system storage type is unsafe")
	}
	if err := os.Chown(sshDirectory, 0, 0); err != nil {
		return err
	}
	return os.Chmod(sshDirectory, 0o700)
}

func EnsureHostKey(path string) (string, error) {
	if err := secureSystemStorage(path); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return "", fmt.Errorf("host key type or mode is unsafe")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return "", fmt.Errorf("host key is not root-owned")
		}
	} else if os.IsNotExist(err) {
		output, commandErr := exec.Command("/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput()
		if commandErr != nil {
			return "", fmt.Errorf("generate host key: %w: %s", commandErr, strings.TrimSpace(string(output)))
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", err
		}
		if err := os.Chmod(path+".pub", 0o644); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	output, err := exec.Command("/usr/bin/ssh-keygen", "-lf", path+".pub", "-E", "sha256").Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		return "", fmt.Errorf("unexpected host key fingerprint")
	}
	return fields[1], nil
}

func CleanupSystemStorage(bootstrap Bootstrap, expectedFingerprint string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("system storage cleanup requires uid 0")
	}
	if expectedFingerprint == "" {
		return fmt.Errorf("expected SSH host fingerprint is required")
	}
	if err := validateRootDirectory(defaultSystemDir, activeSystemDirectoryMode); err != nil {
		return err
	}
	systemEntries, err := os.ReadDir(defaultSystemDir)
	if err != nil {
		return err
	}
	expectedSystemEntries := map[string]bool{"ssh": false, filepath.Base(defaultAuthorizedKeys): false}
	if len(systemEntries) != len(expectedSystemEntries) {
		return fmt.Errorf("system storage contains unexpected entries")
	}
	for _, entry := range systemEntries {
		if _, ok := expectedSystemEntries[entry.Name()]; !ok {
			return fmt.Errorf("system storage contains unexpected entry %q", entry.Name())
		}
		expectedSystemEntries[entry.Name()] = true
	}
	sshDirectory := filepath.Dir(defaultHostKeyPath)
	if err := validateRootDirectory(sshDirectory, 0o700); err != nil {
		return err
	}
	sshEntries, err := os.ReadDir(sshDirectory)
	if err != nil {
		return err
	}
	if len(sshEntries) != 2 {
		return fmt.Errorf("SSH system storage contains unexpected entries")
	}
	expectedNames := map[string]bool{filepath.Base(defaultHostKeyPath): false, filepath.Base(defaultHostKeyPath + ".pub"): false}
	for _, entry := range sshEntries {
		if _, ok := expectedNames[entry.Name()]; !ok {
			return fmt.Errorf("SSH system storage contains unexpected entry %q", entry.Name())
		}
		expectedNames[entry.Name()] = true
	}
	privateKey, err := readRootKeyFile(defaultHostKeyPath, 0o600)
	if err != nil {
		return err
	}
	publicKey, err := readRootKeyFile(defaultHostKeyPath+".pub", 0o644)
	if err != nil {
		return err
	}
	authorizedKey, err := readRootKeyFile(defaultAuthorizedKeys, 0o644)
	if err != nil {
		return err
	}
	bootstrapKey, err := os.ReadFile(bootstrap.AuthorizedKeyPath)
	if err != nil {
		return err
	}
	expectedAuthorizedKey := strings.TrimSpace(string(bootstrapKey)) + "\n"
	if string(authorizedKey) != expectedAuthorizedKey {
		return fmt.Errorf("persisted authorized key identity mismatch")
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("parse persisted SSH host private key: %w", err)
	}
	parsedPublic, _, _, _, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil || parsedPublic.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("parse persisted SSH host public key")
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), parsedPublic.Marshal()) || ssh.FingerprintSHA256(parsedPublic) != expectedFingerprint {
		return fmt.Errorf("persisted SSH host key identity mismatch")
	}
	if err := os.Remove(defaultHostKeyPath); err != nil {
		return err
	}
	if err := os.Remove(defaultHostKeyPath + ".pub"); err != nil {
		return err
	}
	if err := os.Remove(defaultAuthorizedKeys); err != nil {
		return err
	}
	if err := os.Remove(sshDirectory); err != nil {
		return err
	}
	if entries, err := os.ReadDir(defaultSystemDir); err != nil {
		return err
	} else if len(entries) != 0 {
		return fmt.Errorf("system storage is not empty after host-key cleanup")
	}
	if err := os.Chown(defaultSystemDir, bootstrap.OwnerUID, bootstrap.OwnerGID); err != nil {
		return err
	}
	return os.Chmod(defaultSystemDir, 0o700)
}

func validateRootDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != mode || !ok || stat.Uid != 0 {
		return fmt.Errorf("%s directory identity is unsafe", path)
	}
	return nil
}

func readRootKeyFile(path string, mode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != mode || !ok || stat.Uid != 0 || stat.Nlink != 1 {
		return nil, fmt.Errorf("%s file identity is unsafe", path)
	}
	if info.Size() <= 0 || info.Size() > 64<<10 {
		return nil, fmt.Errorf("%s file size is unsafe", path)
	}
	return os.ReadFile(path)
}

func RenderSSHDConfig(hostKey, authorizedKeys string, allowAgentForwarding bool) string {
	agentForwarding := "no"
	if allowAgentForwarding {
		agentForwarding = "yes"
	}
	return fmt.Sprintf(`Port 2222
ListenAddress 127.0.0.1
AddressFamily inet
HostKey %s
AuthorizedKeysFile %s
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
AuthenticationMethods publickey
UsePAM no
PermitRootLogin no
PermitUserEnvironment no
X11Forwarding no
AllowTcpForwarding no
AllowStreamLocalForwarding no
AllowAgentForwarding %s
GatewayPorts no
PermitTunnel no
UseDNS no
AllowUsers agent
PidFile /run/cohotfs/sshd.pid
PrintMotd no
PrintLastLog no
StrictModes yes
Subsystem sftp internal-sftp
`, hostKey, authorizedKeys, agentForwarding)
}

func WriteAndValidateSSHDConfig(path string, bootstrap Bootstrap) error {
	content := RenderSSHDConfig(defaultHostKeyPath, defaultAuthorizedKeys, bootstrap.AllowAgentForwarding)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	output, err := exec.Command("/usr/sbin/sshd", "-t", "-f", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("validate sshd config: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
