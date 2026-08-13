//go:build linux

package containeragent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type SeedManifest struct {
	SchemaVersion int         `json:"schemaVersion"`
	Seeds         []SeedEntry `json:"seeds"`
}

type SeedEntry struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        uint32 `json:"mode"`
}

func InstallSeeds(manifestPath, sourceRoot, home string, uid, gid int) error {
	file, err := os.Open(manifestPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var manifest SeedManifest
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || manifest.SchemaVersion != 1 {
		return fmt.Errorf("invalid agent seed manifest")
	}
	for _, seed := range manifest.Seeds {
		if filepath.IsAbs(seed.Source) || filepath.IsAbs(seed.Destination) || seed.Mode != 0o600 {
			return fmt.Errorf("invalid agent seed path or mode")
		}
		source := filepath.Join(sourceRoot, seed.Source)
		destination := filepath.Join(home, seed.Destination)
		if !beneathPath(source, sourceRoot) || !beneathPath(destination, home) {
			return fmt.Errorf("agent seed path escapes its root")
		}
		info, err := os.Lstat(source)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("agent seed source is unsafe")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		input, err := unix.Open(source, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		output, err := unix.Open(destination, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, seed.Mode)
		if err != nil {
			_ = unix.Close(input)
			return err
		}
		inputFile := os.NewFile(uintptr(input), source)
		outputFile := os.NewFile(uintptr(output), destination)
		copied, copyErr := io.Copy(outputFile, io.LimitReader(inputFile, 1<<20+1))
		if copyErr == nil && copied > 1<<20 {
			copyErr = fmt.Errorf("agent seed exceeds 1 MiB")
		}
		if copyErr == nil {
			copyErr = unix.Fchown(output, uid, gid)
		}
		if copyErr == nil {
			copyErr = outputFile.Sync()
		}
		closeErr := outputFile.Close()
		inputCloseErr := inputFile.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
	}
	return nil
}

func beneathPath(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
