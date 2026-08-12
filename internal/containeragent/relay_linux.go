//go:build linux

package containeragent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

const defaultSSHRelaySocket = "/run/cohotfs/host/ssh/ssh.sock"

func RunSSHRelay(ctx context.Context, socketPath, target string, ownerUID, ownerGID int) error {
	if !filepath.IsAbs(socketPath) {
		return fmt.Errorf("SSH relay socket path must be absolute")
	}
	if _, err := os.Lstat(socketPath); err == nil {
		return fmt.Errorf("SSH relay socket path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	if ownerUID <= 0 || ownerGID <= 0 {
		return fmt.Errorf("SSH relay owner identity is required")
	}
	if err := os.Chown(socketPath, ownerUID, ownerGID); err != nil {
		return err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func(source *net.UnixConn) {
			defer source.Close()
			destination, err := net.Dial("tcp4", target)
			if err != nil {
				return
			}
			defer destination.Close()
			proxyStreams(source, destination)
		}(connection)
	}
}

func proxyStreams(unixConnection *net.UnixConn, tcpConnection net.Conn) {
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(tcpConnection, unixConnection)
		if closeWriter, ok := tcpConnection.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(unixConnection, tcpConnection)
		_ = unixConnection.CloseWrite()
	}()
	wait.Wait()
}
