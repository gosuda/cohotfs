//go:build linux

package containeragent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultSeedManifest = "/run/cohotfs/bootstrap/seeds.json"
	defaultSeedRoot     = "/run/cohotfs/agent-seeds"
	defaultCDPSocket    = "/run/cohotfs/host/cdp.sock"
	defaultGitSocket    = "/run/cohotfs/host/git.sock"
	defaultSecretSocket = "/run/cohotfs/host/secret.sock"
)

func installConfiguredIntegrations(bootstrap Bootstrap) error {
	if _, err := os.Stat(defaultSeedManifest); err == nil {
		if err := InstallSeeds(defaultSeedManifest, defaultSeedRoot, "/home/agent", bootstrap.OwnerUID, bootstrap.OwnerGID); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bootstrap.EnableGitCredentials {
		return writeGitCredentialConfig(bootstrap)
	}
	return nil
}
func writeGitCredentialConfig(bootstrap Bootstrap) error {
	path := "/home/agent/.gitconfig"
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private Git credential config: %w", err)
	}
	content := "[credential]\n\thelper = !/usr/local/libexec/cohotfs-agent git-credential --socket " + defaultGitSocket + " --workspace " + bootstrap.WorkspaceID + "\n\tuseHttpPath = true\n"
	if _, err = file.WriteString(content); err == nil {
		err = file.Chown(bootstrap.OwnerUID, bootstrap.OwnerGID)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func cleanAgentDestination(destination string) (string, error) {
	if destination == "" || filepath.IsAbs(destination) || filepath.Clean(destination) != destination || destination == "." || destination == ".." {
		return "", fmt.Errorf("invalid agent destination")
	}
	return destination, nil
}
