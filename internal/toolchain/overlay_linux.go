//go:build linux

package toolchain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/hostroot"
	"github.com/gosuda/cohotfs/internal/reconcile"
	"github.com/gosuda/cohotfs/internal/runtime"
	"github.com/gosuda/cohotfs/internal/state"
	"golang.org/x/sys/unix"
)

var errOverlayUnavailable = errors.New("unprivileged OverlayFS is unavailable")

func ProbeOverlay(root *hostroot.Root) (available bool, returnErr error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return false, err
	}
	relative := filepath.Join("run", "overlay-probe-"+hex.EncodeToString(random[:]))
	for _, directory := range []string{"lower", "upper", "work", "merged"} {
		if err := root.EnsureDir(filepath.Join(relative, directory), 0o700); err != nil {
			return false, err
		}
	}
	defer func() {
		if err := root.RemoveTree(relative); err != nil {
			available = false
			returnErr = errors.Join(returnErr, fmt.Errorf("remove OverlayFS probe: %w", err))
		}
	}()
	if err := root.AtomicWrite(filepath.Join(relative, "lower", "marker"), []byte("lower\n"), 0o600); err != nil {
		return false, err
	}
	base, _ := root.HostPath(relative)
	overlay := OverlayMount{Lower: filepath.Join(base, "lower"), Upper: filepath.Join(base, "upper"), Work: filepath.Join(base, "work"), Merged: filepath.Join(base, "merged")}
	if err := mountKernelOverlay(overlay); err != nil {
		if overlayUnavailable(err) {
			return false, nil
		}
		return false, err
	}
	mounted := true
	defer func() {
		if mounted {
			if err := unix.Unmount(overlay.Merged, 0); err != nil {
				available = false
				returnErr = errors.Join(returnErr, fmt.Errorf("unmount OverlayFS probe: %w", err))
			}
		}
	}()
	probeFile := filepath.Join(overlay.Merged, "probe")
	if err := os.WriteFile(probeFile, []byte("probe"), 0o600); err != nil {
		return false, nil
	}
	if err := unix.Setxattr(probeFile, "user.cohotfs", []byte("probe"), 0); err != nil {
		return false, nil
	}
	if err := os.Rename(probeFile, filepath.Join(overlay.Merged, "renamed")); err != nil {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(overlay.Lower, "renamed")); !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err := unix.Unmount(overlay.Merged, 0); err != nil {
		return false, fmt.Errorf("unmount OverlayFS probe: %w", err)
	}
	mounted = false
	return true, nil
}

func Activate(plan *Plan, requireCOW bool) ([]state.ExternalResource, error) {
	resources := make([]state.ExternalResource, 0, len(plan.Overlays))
	active := make([]OverlayMount, 0, len(plan.Overlays))
	for _, overlay := range plan.Overlays {
		if err := mountKernelOverlay(overlay); err != nil {
			if _, found, inspectErr := reconcile.FindMount(overlay.Merged); inspectErr != nil || found {
				cleanupErr := Deactivate(resources)
				return nil, errors.Join(fmt.Errorf("refuse fallback with occupied COW mountpoint %s: %w", overlay.Merged, err), inspectErr, cleanupErr)
			}
			if requireCOW {
				cleanupErr := Deactivate(resources)
				return nil, errors.Join(fmt.Errorf("activate COW cache %s: %w", overlay.Target, err), cleanupErr)
			}
			replaceMountSource(plan.Mounts, overlay.Target, overlay.Fallback)
			plan.Fallbacks = append(plan.Fallbacks, overlay.Target+": OverlayFS activation failed; isolated cache")
			continue
		}
		identity, found, err := reconcile.FindMount(overlay.Merged)
		if err != nil || !found || identity.Filesystem != "overlay" {
			if err == nil {
				err = fmt.Errorf("mounted OverlayFS identity unavailable")
			}
			unmountErr := unix.Unmount(overlay.Merged, 0)
			if unmountErr != nil {
				cleanupErr := Deactivate(resources)
				return nil, errors.Join(err, fmt.Errorf("unmount unidentified OverlayFS: %w", unmountErr), cleanupErr)
			}
			if requireCOW {
				cleanupErr := Deactivate(resources)
				return nil, errors.Join(err, cleanupErr)
			}
			replaceMountSource(plan.Mounts, overlay.Target, overlay.Fallback)
			plan.Fallbacks = append(plan.Fallbacks, overlay.Target+": OverlayFS identity unavailable; isolated cache")
			continue
		}
		resources = append(resources, mountResource(identity))
		active = append(active, overlay)
	}
	plan.Overlays = active
	return resources, nil
}

func Deactivate(resources []state.ExternalResource) error {
	var failures []error
	for index := len(resources) - 1; index >= 0; index-- {
		resource := resources[index]
		if resource.Type != "mount" || resource.ReleasedAt != nil {
			continue
		}
		identity, err := recordedMountIdentity(resource)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := reconcile.ValidateMount(identity); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			failures = append(failures, fmt.Errorf("refuse unmount of changed resource %s: %w", resource.ID, err))
			continue
		}
		if err := unix.Unmount(identity.MountPoint, 0); err != nil {
			failures = append(failures, fmt.Errorf("unmount %s: %w", identity.MountPoint, err))
		}
	}
	return errors.Join(failures...)
}

func ValidateResources(resources []state.ExternalResource) error {
	for _, resource := range resources {
		if resource.Type != "mount" || resource.ReleasedAt != nil {
			continue
		}
		identity, err := recordedMountIdentity(resource)
		if err != nil {
			return err
		}
		if err := reconcile.ValidateMount(identity); err != nil {
			return fmt.Errorf("toolchain mount %s: %w", resource.ID, err)
		}
	}
	return nil
}

func mountKernelOverlay(overlay OverlayMount) error {
	for _, path := range []string{overlay.Lower, overlay.Upper, overlay.Work, overlay.Merged} {
		if strings.ContainsAny(path, ",:") {
			return fmt.Errorf("OverlayFS path contains an unsupported delimiter")
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("OverlayFS path %s is not a directory", path)
		}
	}
	if mounted, _, err := reconcile.FindMount(overlay.Merged); err != nil {
		return err
	} else if mounted.MountPoint != "" {
		return fmt.Errorf("OverlayFS target is already mounted")
	}
	if err := requireEmpty(overlay.Work); err != nil {
		return err
	}
	if err := requireSameFilesystem(overlay.Upper, overlay.Work); err != nil {
		return err
	}
	options := "lowerdir=" + overlay.Lower + ",upperdir=" + overlay.Upper + ",workdir=" + overlay.Work + ",userxattr"
	flags := uintptr(unix.MS_NODEV | unix.MS_NOSUID)
	if !overlay.Executable {
		flags |= unix.MS_NOEXEC
	}
	if err := unix.Mount("overlay", overlay.Merged, "overlay", flags, options); err != nil {
		return errors.Join(errOverlayUnavailable, err)
	}
	return nil
}

func requireEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("OverlayFS work directory is not empty")
	}
	return nil
}

func requireSameFilesystem(first, second string) error {
	var firstStat, secondStat syscall.Stat_t
	if err := syscall.Stat(first, &firstStat); err != nil {
		return err
	}
	if err := syscall.Stat(second, &secondStat); err != nil {
		return err
	}
	if firstStat.Dev != secondStat.Dev {
		return fmt.Errorf("OverlayFS upper and work directories are on different filesystems")
	}
	return nil
}

func replaceMountSource(mounts []runtime.Mount, target, source string) {
	for index := range mounts {
		if mounts[index].Target == target {
			mounts[index].Source = source
			return
		}
	}
}

func mountResource(identity reconcile.MountIdentity) state.ExternalResource {
	return state.ExternalResource{Type: "mount", ID: identity.MountPoint, AcquiredAt: time.Now().UTC(), Identity: map[string]string{
		"mountID": strconv.Itoa(identity.MountID), "majorMinor": identity.MajorMinor, "root": identity.Root,
		"mountPoint": identity.MountPoint, "filesystem": identity.Filesystem, "source": identity.Source,
	}}
}

func recordedMountIdentity(resource state.ExternalResource) (reconcile.MountIdentity, error) {
	id, err := strconv.Atoi(resource.Identity["mountID"])
	if err != nil {
		return reconcile.MountIdentity{}, err
	}
	return reconcile.MountIdentity{MountID: id, MajorMinor: resource.Identity["majorMinor"], Root: resource.Identity["root"], MountPoint: resource.Identity["mountPoint"], Filesystem: resource.Identity["filesystem"], Source: resource.Identity["source"]}, nil
}

func overlayUnavailable(err error) bool {
	return errors.Is(err, errOverlayUnavailable) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EINVAL)
}
