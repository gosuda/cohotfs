//go:build linux

// Package setup validates and executes repository setup contracts.
package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gosuda/cohotfs/internal/config"
)

type Validation struct {
	Command []string      `json:"command"`
	User    string        `json:"user"`
	Timeout time.Duration `json:"timeout"`
	Digest  string        `json:"digest"`
	Script  string        `json:"script"`
}

type validationAudit struct {
	Setup        config.SetupSpec `json:"setup"`
	ImageDigest  string           `json:"imageDigest"`
	BootstrapAPI string           `json:"bootstrapAPI"`
	OwnerUID     int              `json:"ownerUID"`
	OwnerGID     int              `json:"ownerGID"`
	ScriptPath   string           `json:"scriptPath,omitempty"`
	ScriptSHA256 string           `json:"scriptSHA256,omitempty"`
	ScriptDevice uint64           `json:"scriptDevice,omitempty"`
	ScriptInode  uint64           `json:"scriptInode,omitempty"`
}

func Validate(source string, spec config.SetupSpec, imageDigest, bootstrapAPI string, ownerUID, ownerGID int) (Validation, error) {
	if len(spec.Command) == 0 || spec.Command[0] == "" || spec.Timeout <= 0 {
		return Validation{}, fmt.Errorf("setup command must be a non-empty argv and timeout must be positive")
	}
	audit := validationAudit{
		Setup: spec, ImageDigest: imageDigest, BootstrapAPI: bootstrapAPI,
		OwnerUID: ownerUID, OwnerGID: ownerGID,
	}
	if len(spec.Command) == 1 && spec.Command[0] == "/bin/true" {
		return completeValidation(spec, ownerUID, ownerGID, audit)
	}
	canonicalSource, err := filepath.Abs(source)
	if err != nil {
		return Validation{}, err
	}
	canonicalSource, err = filepath.EvalSymlinks(canonicalSource)
	if err != nil {
		return Validation{}, err
	}
	scriptIndex := -1
	for index, argument := range spec.Command {
		if index == 0 && filepath.IsAbs(argument) {
			continue
		}
		if strings.Contains(argument, string(filepath.Separator)) || strings.HasSuffix(argument, ".sh") {
			scriptIndex = index
			break
		}
	}
	if scriptIndex < 0 {
		return Validation{}, fmt.Errorf("setup argv must identify a repository script")
	}
	scriptArgument := spec.Command[scriptIndex]
	if filepath.IsAbs(scriptArgument) {
		return Validation{}, fmt.Errorf("setup script must be relative to the workspace source")
	}
	scriptPath := filepath.Join(canonicalSource, scriptArgument)
	originalInfo, err := os.Lstat(scriptPath)
	if err != nil {
		return Validation{}, err
	}
	if !originalInfo.Mode().IsRegular() || originalInfo.Mode()&os.ModeSymlink != 0 || originalInfo.Mode().Perm()&0o002 != 0 {
		return Validation{}, fmt.Errorf("setup script must be regular, non-symlink, and not other-writable")
	}
	canonicalScript, err := filepath.EvalSymlinks(scriptPath)
	if err != nil {
		return Validation{}, fmt.Errorf("resolve setup script: %w", err)
	}
	if !beneath(canonicalScript, canonicalSource) {
		return Validation{}, fmt.Errorf("setup script escapes workspace source")
	}
	relativeScript, err := filepath.Rel(canonicalSource, canonicalScript)
	if err != nil {
		return Validation{}, err
	}
	for _, reserved := range []string{".cohotfs", ".omp"} {
		if relativeScript == reserved || strings.HasPrefix(relativeScript, reserved+string(filepath.Separator)) {
			return Validation{}, fmt.Errorf("setup script is hidden by reserved workspace path %s", reserved)
		}
	}
	file, err := os.OpenFile(scriptPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return Validation{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return Validation{}, err
	}
	originalStat, originalOK := originalInfo.Sys().(*syscall.Stat_t)
	openedStat, openedOK := openedInfo.Sys().(*syscall.Stat_t)
	if !originalOK || !openedOK {
		return Validation{}, fmt.Errorf("setup script identity unavailable")
	}
	if originalStat.Dev != openedStat.Dev || originalStat.Ino != openedStat.Ino {
		return Validation{}, fmt.Errorf("setup script changed during validation")
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return Validation{}, err
	}
	audit.ScriptPath = canonicalScript
	audit.ScriptSHA256 = digest(content)
	audit.ScriptDevice = uint64(openedStat.Dev)
	audit.ScriptInode = openedStat.Ino
	return completeValidation(spec, ownerUID, ownerGID, audit)
}

func completeValidation(spec config.SetupSpec, ownerUID, ownerGID int, audit validationAudit) (Validation, error) {
	raw, err := json.Marshal(audit)
	if err != nil {
		return Validation{}, err
	}
	return Validation{
		Command: append([]string(nil), spec.Command...),
		User:    fmt.Sprintf("%d:%d", ownerUID, ownerGID),
		Timeout: spec.Timeout,
		Digest:  digest(raw),
		Script:  audit.ScriptPath,
	}, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func beneath(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
