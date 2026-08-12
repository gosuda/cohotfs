//go:build linux

package hostservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/gosuda/cohotfs/internal/api"
	"github.com/gosuda/cohotfs/internal/proc"
	"github.com/gosuda/cohotfs/internal/reconcile"
	"golang.org/x/sys/unix"
)

const leaseRecordPath = "run/host-leases.json"

type ManagedProcess struct {
	Identity     proc.Identity `json:"identity"`
	ProcessGroup int           `json:"processGroup"`
}

type durableLease struct {
	Summary        api.LeaseSummary          `json:"summary"`
	IdempotencyKey string                    `json:"idempotencyKey"`
	RequestDigest  string                    `json:"requestDigest"`
	Processes      []ManagedProcess          `json:"processes,omitempty"`
	Sockets        []proc.SocketIdentity     `json:"sockets,omitempty"`
	Mounts         []reconcile.MountIdentity `json:"mounts,omitempty"`
}

type durableLeaseFile struct {
	SchemaVersion int            `json:"schemaVersion"`
	Host          proc.Identity  `json:"host"`
	Leases        []durableLease `json:"leases"`
}

func (s *Server) persistLeasesLocked() error {
	leases := make([]durableLease, 0, len(s.leases))
	for _, lease := range s.leases {
		leases = append(leases, durableLease{
			Summary: lease.Summary, IdempotencyKey: lease.IdempotencyKey,
			RequestDigest: lease.RequestDigest,
			Processes:     lease.Processes, Sockets: lease.Sockets, Mounts: lease.Mounts,
		})
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].Summary.LeaseID < leases[j].Summary.LeaseID })
	if len(leases) == 0 {
		if err := s.root.Remove(leaseRecordPath); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	record := durableLeaseFile{SchemaVersion: 1, Host: s.identity, Leases: leases}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return s.root.AtomicWrite(leaseRecordPath, append(data, '\n'), 0o600)
}

func (s *Server) reconcileStaleLeases(ctx context.Context) error {
	data, err := s.root.ReadFile(leaseRecordPath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	var record durableLeaseFile
	if err := json.Unmarshal(data, &record); err != nil || record.SchemaVersion != 1 {
		return fmt.Errorf("stale host lease record is invalid")
	}
	if record.Host.PID == s.identity.PID && record.Host.StartTicks == s.identity.StartTicks {
		return nil
	}
	if err := proc.Matches(record.Host); err == nil {
		return fmt.Errorf("recorded host service is still running")
	}
	var failures []error
	for _, lease := range record.Leases {
		for _, socket := range lease.Sockets {
			if proc.ValidateSocket(socket) == nil {
				if err := os.Remove(socket.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
					failures = append(failures, err)
				}
			}
		}
		for _, mount := range lease.Mounts {
			if reconcile.ValidateMount(mount) == nil {
				if err := unix.Unmount(mount.MountPoint, 0); err != nil {
					failures = append(failures, err)
				}
			}
		}
		for _, process := range lease.Processes {
			if err := terminateManagedProcess(ctx, process); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("stale lease cleanup is ambiguous: %v", failures)
	}
	return s.root.Remove(leaseRecordPath)
}

func terminateManagedProcess(ctx context.Context, process ManagedProcess) error {
	if err := proc.Matches(process.Identity); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("process identity mismatch: %w", err)
	}
	group, err := unix.Getpgid(process.Identity.PID)
	if err != nil {
		return err
	}
	if group != process.ProcessGroup || process.ProcessGroup != process.Identity.PID {
		return fmt.Errorf("process group identity mismatch")
	}
	if err := unix.Kill(-process.ProcessGroup, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if err := proc.Matches(process.Identity); err == nil {
				return unix.Kill(-process.ProcessGroup, unix.SIGKILL)
			}
			return nil
		case <-ticker.C:
			if err := proc.Matches(process.Identity); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return fmt.Errorf("process changed during cleanup: %w", err)
			}
		}
	}
}
