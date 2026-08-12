//go:build linux

// Package sshproxy implements the raw-byte OpenSSH ProxyCommand transport.
package sshproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/state"
)

func Proxy(ctx context.Context, store *state.Store, id string, stdin io.Reader, stdout io.Writer) error {
	record, err := store.LoadWorkspace(id)
	if err != nil {
		return err
	}
	if record.Status != state.StatusReady {
		return fmt.Errorf("workspace is not ready")
	}
	connection, err := dialRecorded(ctx, record)
	if err != nil {
		return err
	}
	defer connection.Close()
	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(connection, stdin)
		if closeWriter, ok := connection.(interface{ CloseWrite() error }); ok {
			copyErr = errors.Join(copyErr, closeWriter.CloseWrite())
		}
		inputDone <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(stdout, connection)
		outputDone <- copyErr
	}()
	select {
	case copyErr := <-outputDone:
		return copyErr
	case copyErr := <-inputDone:
		if copyErr != nil {
			return copyErr
		}
		select {
		case copyErr := <-outputDone:
			return copyErr
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func dialRecorded(ctx context.Context, record state.Workspace) (net.Conn, error) {
	for _, resource := range record.Resources {
		if resource.ReleasedAt != nil || resource.Quarantined {
			continue
		}
		switch resource.Type {
		case "ssh_socket":
			if resource.Identity["nonce"] == "" || resource.Identity["nonce"] != record.RuntimeRef.Nonce {
				return nil, fmt.Errorf("SSH socket runtime nonce mismatch")
			}
			uid, err := strconv.Atoi(resource.Identity["uid"])
			if err != nil {
				return nil, err
			}
			dev, err := strconv.ParseUint(resource.Identity["dev"], 10, 64)
			if err != nil {
				return nil, err
			}
			inode, err := strconv.ParseUint(resource.Identity["inode"], 10, 64)
			if err != nil {
				return nil, err
			}
			mode, err := strconv.ParseUint(resource.Identity["mode"], 10, 32)
			if err != nil {
				return nil, err
			}
			identity := proc.SocketIdentity{Path: resource.Identity["path"], UID: uid, Dev: dev, Inode: inode, Mode: uint32(mode)}
			if err := proc.ValidateSocket(identity); err != nil {
				return nil, fmt.Errorf("SSH socket identity mismatch: %w", err)
			}
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "unix", identity.Path)
		}
	}
	return nil, os.ErrNotExist
}
