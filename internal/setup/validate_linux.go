//go:build linux

// Package setup validates and executes repository setup contracts.
package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func Validate(source string, spec config.SetupSpec, imageDigest, bootstrapAPI string, ownerUID, ownerGID int) (Validation, error) {
	if len(spec.Command) == 0 || spec.Command[0] == "" || spec.Timeout <= 0 {
		return Validation{}, fmt.Errorf("setup command must be a non-empty argv and timeout must be positive")
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
	canonicalScript, err := filepath.EvalSymlinks(scriptPath)
	if err != nil {
		return Validation{}, fmt.Errorf("resolve setup script: %w", err)
	}
	if !beneath(canonicalScript, canonicalSource) {
		return Validation{}, fmt.Errorf("setup script escapes workspace source")
	}
	info, err := os.Lstat(canonicalScript)
	if err != nil {
		return Validation{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o002 != 0 {
		return Validation{}, fmt.Errorf("setup script must be regular, non-symlink, and not other-writable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Validation{}, fmt.Errorf("setup script identity unavailable")
	}
	content, err := os.ReadFile(canonicalScript)
	if err != nil {
		return Validation{}, err
	}
	audit := struct {
		Setup        config.SetupSpec `json:"setup"`
		ImageDigest  string           `json:"imageDigest"`
		BootstrapAPI string           `json:"bootstrapAPI"`
		OwnerUID     int              `json:"ownerUID"`
		OwnerGID     int              `json:"ownerGID"`
		ScriptPath   string           `json:"scriptPath"`
		ScriptSHA256 string           `json:"scriptSHA256"`
		ScriptDevice uint64           `json:"scriptDevice"`
		ScriptInode  uint64           `json:"scriptInode"`
	}{spec, imageDigest, bootstrapAPI, ownerUID, ownerGID, canonicalScript, digest(content), uint64(stat.Dev), stat.Ino}
	raw, err := json.Marshal(audit)
	if err != nil {
		return Validation{}, err
	}
	return Validation{Command: append([]string(nil), spec.Command...), User: fmt.Sprintf("%d:%d", ownerUID, ownerGID), Timeout: spec.Timeout, Digest: digest(raw), Script: canonicalScript}, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func beneath(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
