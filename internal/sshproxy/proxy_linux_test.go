//go:build linux

package sshproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
)

func TestProxyDirectorySocketPreservesRawBinary(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "ssh.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer connection.Close()
		buffer := make([]byte, 32<<10)
		for {
			count, err := connection.Read(buffer)
			if count != 0 {
				_, _ = connection.Write(buffer[:count])
			}
			if err != nil {
				return
			}
		}
	}()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, err := state.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := state.NewWorkspaceID()
	record := state.Workspace{ID: id, Status: state.StatusReady, RuntimeRef: runtime.WorkspaceRef{Nonce: "nonce"}, Resources: []state.ExternalResource{socketExternalResource(t, socketPath)}}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 128<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Proxy(context.Background(), store, id, bytes.NewReader(payload), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), payload) {
		t.Fatalf("proxy changed stream: got %d want %d", output.Len(), len(payload))
	}
}

func TestProxyRejectsReplacedDirectorySocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "ssh.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	resource := socketExternalResource(t, socketPath)
	inode, err := strconv.ParseUint(resource.Identity["inode"], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	resource.Identity["inode"] = strconv.FormatUint(inode+1, 10)
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, _ := state.NewStore(root)
	id, _ := state.NewWorkspaceID()
	record := state.Workspace{ID: id, Status: state.StatusReady, RuntimeRef: runtime.WorkspaceRef{Nonce: "nonce"}, Resources: []state.ExternalResource{resource}}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	if err := Proxy(context.Background(), store, id, bytes.NewReader(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted replaced SSH socket")
	}
}

func TestProxyRejectsRuntimeNonceMismatch(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "ssh.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, _ := state.NewStore(root)
	id, _ := state.NewWorkspaceID()
	record := state.Workspace{ID: id, Status: state.StatusReady, RuntimeRef: runtime.WorkspaceRef{Nonce: "replaced"}, Resources: []state.ExternalResource{socketExternalResource(t, socketPath)}}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	if err := Proxy(context.Background(), store, id, bytes.NewReader(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted replaced runtime nonce")
	}
}

func TestProxyReturnsWhenRemoteClosesWhileStdinRemainsOpen(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "ssh.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_ = connection.Close()
		}
	}()
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	store, _ := state.NewStore(root)
	id, _ := state.NewWorkspaceID()
	record := state.Workspace{ID: id, Status: state.StatusReady, RuntimeRef: runtime.WorkspaceRef{Nonce: "nonce"}, Resources: []state.ExternalResource{socketExternalResource(t, socketPath)}}
	if err := store.SaveWorkspace(record); err != nil {
		t.Fatal(err)
	}
	stdin, keepOpen := io.Pipe()
	defer stdin.Close()
	defer keepOpen.Close()
	done := make(chan error, 1)
	go func() { done <- Proxy(context.Background(), store, id, stdin, &bytes.Buffer{}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy waited for stdin after remote close")
	}
}

func socketExternalResource(t *testing.T, path string) state.ExternalResource {
	t.Helper()
	identity, err := proc.ReadSocket(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	return state.ExternalResource{
		Type: "ssh_socket", ID: path,
		Identity: map[string]string{
			"path": path, "uid": strconv.Itoa(identity.UID), "dev": strconv.FormatUint(identity.Dev, 10),
			"inode": strconv.FormatUint(identity.Inode, 10), "mode": strconv.FormatUint(uint64(identity.Mode), 10), "nonce": "nonce",
		},
	}
}
