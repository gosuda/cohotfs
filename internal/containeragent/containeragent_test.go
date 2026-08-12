//go:build linux

package containeragent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckImageContract(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"/.cohotfs", "/usr/local/libexec", "/usr/sbin", "/bin"} {
		if err := os.MkdirAll(rooted(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := BaseMarker{APIVersion: "cohotfs.io/v1alpha1", Kind: "CohotfsBase", BootstrapAPI: BootstrapAPI, OS: "linux", Architecture: "amd64", AgentPath: "/usr/local/libexec/cohotfs-agent"}
	raw, _ := json.Marshal(marker)
	if err := os.WriteFile(rooted(root, "/.cohotfs/base.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{marker.AgentPath, "/usr/sbin/sshd", "/bin/sh"} {
		if err := os.WriteFile(rooted(root, path), []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(rooted(root, "/bin/sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rooted(root, "/bin/dash"), []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dash", rooted(root, "/bin/sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckImage(root, BootstrapAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckImage(root, "future"); err == nil || !strings.Contains(err.Error(), "image_incompatible") {
		t.Fatalf("wrong API error = %v", err)
	}
	if err := os.Remove(rooted(root, marker.AgentPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/true", rooted(root, marker.AgentPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckImage(root, BootstrapAPI); err == nil {
		t.Fatal("accepted symlink agent executable")
	}
	if err := os.Remove(rooted(root, marker.AgentPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rooted(root, marker.AgentPath), []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rooted(root, "/usr/sbin/sshd")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "sshd")
	if err := os.WriteFile(outside, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, rooted(root, "/usr/sbin/sshd")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckImage(root, BootstrapAPI); err == nil {
		t.Fatal("accepted system executable symlink outside image root")
	}
}

func TestExecuteRejectsManagedToolchainEnvironmentOverride(t *testing.T) {
	t.Setenv("COHOTFS_MANAGED_TOOLCHAINS", "1")
	t.Setenv("GOMODCACHE", "/host/cache")
	var stdout, stderr bytes.Buffer
	if err := Execute(context.Background(), []string{"version"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "GOMODCACHE") {
		t.Fatalf("override error = %v", err)
	}
}

func TestProbeReadinessRequiresLiveValidatedProtocol(t *testing.T) {
	ready := Ready{
		BootstrapAPI:       BootstrapAPI,
		SSHAddress:         "127.0.0.1:2222",
		SSHHostFingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
		SSHRelay:           true,
		TCPForwarding:      true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	socket := filepath.Join(t.TempDir(), "ready.sock")
	result := make(chan error, 1)
	go func() { result <- serveReadiness(ctx, socket, ready) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness socket was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := ProbeReadiness(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	if got != ready {
		t.Fatalf("readiness = %#v, want %#v", got, ready)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readiness server did not stop")
	}

	stale := filepath.Join(t.TempDir(), "stale.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeReadiness(context.Background(), stale); err == nil {
		t.Fatal("accepted stale readiness socket pathname")
	}
	recoveryCtx, recoveryCancel := context.WithCancel(context.Background())
	recoveryResult := make(chan error, 1)
	go func() { recoveryResult <- serveReadiness(recoveryCtx, stale, ready) }()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if _, err := ProbeReadiness(context.Background(), stale); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness server did not recover stale socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	recoveryCancel()
	select {
	case err := <-recoveryResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovered readiness server did not stop")
	}
}

func TestDecodeReadyRejectsMalformedProtocol(t *testing.T) {
	fingerprint := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for _, raw := range []string{
		`{"bootstrapAPI":"future","sshAddress":"127.0.0.1:2222","sshHostFingerprint":"` + fingerprint + `"}`,
		`{"bootstrapAPI":"v1alpha2","sshAddress":"127.0.0.1:2222","sshHostFingerprint":"invalid","sshRelay":true,"tcpForwarding":true}`,
		`{"bootstrapAPI":"v1alpha2","sshAddress":"127.0.0.1:2222","sshHostFingerprint":"` + fingerprint + `","sshRelay":true,"tcpForwarding":true,"unknown":true}`,
	} {
		if _, err := DecodeReady(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted malformed readiness response %s", raw)
		}
	}
}

func TestBootstrapStrictnessAndIdentityConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	for _, raw := range []string{
		`{"bootstrapAPI":"v1alpha2","workspaceID":"aaaaaaaaaaaaaaaaaaaaaaaaaa","ownerUID":1000,"ownerGID":1000,"authorizedKeyPath":"/run/cohotfs/bootstrap/key.pub","unexpected":true}`,
		`{"bootstrapAPI":"v1alpha2","bootstrapAPI":"v1alpha2","workspaceID":"aaaaaaaaaaaaaaaaaaaaaaaaaa","ownerUID":1000,"ownerGID":1000,"authorizedKeyPath":"/run/cohotfs/bootstrap/key.pub"}`,
		`{"bootstrapAPI":"v1alpha2","workspaceID":"aaaaaaaaaaaaaaaaaaaaaaaaaa","ownerUID":1000,"ownerGID":1000,"authorizedKeyPath":"/run/cohotfs/bootstrap/key.pub"} {}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBootstrap(path); err == nil {
			t.Fatalf("accepted malformed bootstrap %s", raw)
		}
	}
	users := []passwdEntry{{Name: "developer", UID: 1000, GID: 1000, Home: "/home/developer", Shell: "/bin/sh"}}
	if _, _, err := validateIdentity(users, nil, 1000, 1000); err == nil {
		t.Fatal("accepted conflicting uid")
	}
	users = []passwdEntry{{Name: "agent", UID: 1000, GID: 1000, Home: "/home/agent", Shell: "/bin/sh"}}
	groups := []groupEntry{{Name: "agent", GID: 1000}}
	userExists, groupExists, err := validateIdentity(users, groups, 1000, 1000)
	if err != nil || !userExists || !groupExists {
		t.Fatalf("matching identity = %v/%v, %v", userExists, groupExists, err)
	}
}

func TestAuthorizedKeyAndSSHDConfig(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "key.pub")
	destination := filepath.Join(directory, "system", "authorized_keys")
	blob := make([]byte, 64)
	key := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " fixture\n"
	if err := os.WriteFile(source, []byte(key), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallAuthorizedKey(source, destination); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(destination)
	if err != nil || string(installed) != key {
		t.Fatalf("installed key = %q, %v", installed, err)
	}
	config := RenderSSHDConfig("/key", "/authorized", false)
	for _, required := range []string{"ListenAddress 127.0.0.1", "PermitRootLogin no", "PasswordAuthentication no", "AllowTcpForwarding local", "AllowStreamLocalForwarding no", "PermitOpen 127.0.0.1:*", "AllowAgentForwarding no", "AllowUsers agent"} {
		if !strings.Contains(config, required) {
			t.Fatalf("sshd config missing %q", required)
		}
	}
	if strings.Contains(config, "AllowAgentForwarding yes") {
		t.Fatal("agent forwarding enabled without grant")
	}
	if !strings.Contains(RenderSSHDConfig("/key", "/authorized", true), "AllowAgentForwarding yes") {
		t.Fatal("agent forwarding grant not rendered")
	}
}

func TestSSHRelayPreservesBinaryStream(t *testing.T) {
	tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	go func() {
		connection, err := tcpListener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(t.TempDir(), "ssh.sock")
	result := make(chan error, 1)
	go func() { result <- RunSSHRelay(ctx, socket, tcpListener.Addr().String(), os.Getuid(), os.Getgid()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("relay mode = %04o", info.Mode().Perm())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay readiness timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 64<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	echoed, err := io.ReadAll(connection)
	_ = connection.Close()
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("relay changed binary stream: got=%d want=%d err=%v", len(echoed), len(payload), err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop")
	}
}
