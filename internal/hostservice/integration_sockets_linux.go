//go:build linux

package hostservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gosuda/cohotfs/internal/proc"
)

type leaseSocket struct {
	identity proc.SocketIdentity
	close    func(context.Context) error
}

func (s *Server) newLeaseHTTPServer(relative string, handler http.Handler) (leaseSocket, error) {
	listener, identity, err := s.listenLeaseSocket(relative)
	if err != nil {
		return leaseSocket{}, err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			uid, peerErr := peerUID(connection)
			if peerErr != nil {
				uid = -1
			}
			return context.WithValue(ctx, peerUIDKey{}, uid)
		},
	}
	go func() {
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			_ = listener.Close()
		}
	}()
	var once sync.Once
	closeSocket := func(ctx context.Context) error {
		var result error
		once.Do(func() {
			result = errors.Join(server.Shutdown(ctx), removeLeaseSocket(identity))
		})
		return result
	}
	return leaseSocket{identity: identity, close: closeSocket}, nil
}

func (s *Server) newLeaseTCPBridge(relative, target string) (leaseSocket, error) {
	listener, identity, err := s.listenLeaseSocket(relative)
	if err != nil {
		return leaseSocket{}, err
	}
	bridgeCtx, cancel := context.WithCancel(context.Background())
	var connections sync.WaitGroup
	var connectionMu sync.Mutex
	openConnections := map[net.Conn]struct{}{}
	track := func(connection net.Conn) {
		connectionMu.Lock()
		openConnections[connection] = struct{}{}
		connectionMu.Unlock()
	}
	untrack := func(connection net.Conn) {
		connectionMu.Lock()
		delete(openConnections, connection)
		connectionMu.Unlock()
	}
	closeOpen := func() {
		connectionMu.Lock()
		defer connectionMu.Unlock()
		for connection := range openConnections {
			_ = connection.Close()
		}
	}
	go func() {
		for {
			client, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				track(client)
				defer func() { untrack(client); _ = client.Close() }()
				upstream, dialErr := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(bridgeCtx, "tcp4", target)
				if dialErr != nil {
					return
				}
				track(upstream)
				defer func() { untrack(upstream); _ = upstream.Close() }()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	var once sync.Once
	closeSocket := func(ctx context.Context) error {
		var result error
		once.Do(func() {
			cancel()
			closeErr := listener.Close()
			closeOpen()
			done := make(chan struct{})
			go func() { connections.Wait(); close(done) }()
			select {
			case <-done:
			case <-ctx.Done():
				result = ctx.Err()
			}
			result = errors.Join(result, closeErr, removeLeaseSocket(identity))
		})
		return result
	}
	return leaseSocket{identity: identity, close: closeSocket}, nil
}

func (s *Server) listenLeaseSocket(relative string) (*net.UnixListener, proc.SocketIdentity, error) {
	if err := s.root.EnsureDir(filepath.Dir(relative), 0o700); err != nil {
		return nil, proc.SocketIdentity{}, err
	}
	path, err := s.root.SocketPath(relative)
	if err != nil {
		return nil, proc.SocketIdentity{}, err
	}
	if err := removeStaleSocket(path, s.root.UID()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, proc.SocketIdentity{}, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, proc.SocketIdentity{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, proc.SocketIdentity{}, err
	}
	identity, err := proc.ReadSocket(path, s.root.UID())
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, proc.SocketIdentity{}, err
	}
	return listener, identity, nil
}

func removeLeaseSocket(identity proc.SocketIdentity) error {
	if err := proc.ValidateSocket(identity); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lease socket identity mismatch: %w", err)
	}
	if err := os.Remove(identity.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func socketLeaseMetadata(identity proc.SocketIdentity) map[string]string {
	return map[string]string{
		"path": identity.Path, "uid": strconv.Itoa(identity.UID), "dev": strconv.FormatUint(identity.Dev, 10),
		"inode": strconv.FormatUint(identity.Inode, 10), "mode": strconv.FormatUint(uint64(identity.Mode), 10),
	}
}
