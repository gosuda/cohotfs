//go:build !linux

package workspace

import (
	"fmt"
	"os"
)

func reservedWorkspaceMaskIdentity(name string, _ os.FileInfo) (ReservedWorkspaceMask, error) {
	return ReservedWorkspaceMask{}, fmt.Errorf("reserved workspace path %s identity is unavailable on this platform", name)
}

func openReservedWorkspaceMask(path string) (*os.File, error) {
	return nil, fmt.Errorf("reserved workspace path %s cannot be pinned on this platform", path)
}
