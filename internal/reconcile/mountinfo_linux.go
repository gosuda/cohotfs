//go:build linux

// Package reconcile validates recorded external-resource identities without
// deleting unrecognized objects.
package reconcile

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type MountIdentity struct {
	MountID    int    `json:"mountID"`
	MajorMinor string `json:"majorMinor"`
	Root       string `json:"root"`
	MountPoint string `json:"mountPoint"`
	Filesystem string `json:"filesystem"`
	Source     string `json:"source"`
}

func ReadMountInfo() ([]MountIdentity, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var mounts []MountIdentity
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		mount, err := parseMountInfoLine(scanner.Text())
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

func FindMount(path string) (MountIdentity, bool, error) {
	mounts, err := ReadMountInfo()
	if err != nil {
		return MountIdentity{}, false, err
	}
	for _, mount := range mounts {
		if mount.MountPoint == path {
			return mount, true, nil
		}
	}
	return MountIdentity{}, false, nil
}

func ValidateMount(recorded MountIdentity) error {
	current, found, err := FindMount(recorded.MountPoint)
	if err != nil {
		return err
	}
	if !found {
		return os.ErrNotExist
	}
	if current.MountID != recorded.MountID || current.MajorMinor != recorded.MajorMinor || current.Root != recorded.Root || current.Filesystem != recorded.Filesystem || current.Source != recorded.Source {
		return fmt.Errorf("mount identity mismatch")
	}
	return nil
}

func parseMountInfoLine(line string) (MountIdentity, error) {
	left, right, ok := strings.Cut(line, " - ")
	if !ok {
		return MountIdentity{}, fmt.Errorf("malformed mountinfo line")
	}
	before := strings.Fields(left)
	after := strings.Fields(right)
	if len(before) < 6 || len(after) < 2 {
		return MountIdentity{}, fmt.Errorf("malformed mountinfo fields")
	}
	id, err := strconv.Atoi(before[0])
	if err != nil {
		return MountIdentity{}, err
	}
	source := ""
	if len(after) >= 2 {
		source = unescapeMountField(after[1])
	}
	return MountIdentity{
		MountID: id, MajorMinor: before[2], Root: unescapeMountField(before[3]),
		MountPoint: unescapeMountField(before[4]), Filesystem: after[0], Source: source,
	}, nil
}

func unescapeMountField(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}
