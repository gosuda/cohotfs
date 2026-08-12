package windowsbridge

import (
	"fmt"
	"regexp"
	"strings"
)

var workspaceIDPattern = regexp.MustCompile(`^[a-z2-7]{26}$`)

type uncPath struct {
	distro     string
	components []string
}

func validateProfileBoundary(modulePath, profileRoot, profile, distro string) error {
	module, err := parseWSLUNC(modulePath)
	if err != nil {
		return fmt.Errorf("Windows companion location: %w", err)
	}
	root, err := parseWSLUNC(profileRoot)
	if err != nil {
		return fmt.Errorf("Windows profile root: %w", err)
	}
	candidate, err := parseWSLUNC(profile)
	if err != nil {
		return fmt.Errorf("Windows profile: %w", err)
	}
	if !strings.EqualFold(module.distro, distro) || !strings.EqualFold(root.distro, distro) || !strings.EqualFold(candidate.distro, distro) {
		return fmt.Errorf("Windows profile distro does not match WSL_DISTRO_NAME")
	}
	if len(module.components) < 3 || !strings.EqualFold(module.components[len(module.components)-2], "bin") || !strings.EqualFold(module.components[len(module.components)-1], "cohotfs-windows-bridge.exe") {
		return fmt.Errorf("Windows companion is not installed under the Cohotfs bin directory")
	}
	expectedRoot := append(append([]string(nil), module.components[:len(module.components)-2]...), "browser")
	if !equalWindowsComponents(root.components, expectedRoot) {
		return fmt.Errorf("Windows profile root does not match the companion Cohotfs root")
	}
	if len(candidate.components) != len(root.components)+1 || !equalWindowsComponents(candidate.components[:len(root.components)], root.components) || !workspaceIDPattern.MatchString(candidate.components[len(candidate.components)-1]) {
		return fmt.Errorf("Windows profile is not one workspace below the Cohotfs browser root")
	}
	return nil
}

func parseWSLUNC(value string) (uncPath, error) {
	value = strings.ReplaceAll(value, "/", `\`)
	const prefix = `\\wsl.localhost\`
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return uncPath{}, fmt.Errorf(`path must use the \\wsl.localhost share`)
	}
	parts := strings.Split(value[len(prefix):], `\`)
	if len(parts) < 2 || parts[0] == "" {
		return uncPath{}, fmt.Errorf("path is missing its distro or path")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, ":\x00") {
			return uncPath{}, fmt.Errorf("path contains an invalid component")
		}
	}
	return uncPath{distro: parts[0], components: parts[1:]}, nil
}

func equalWindowsComponents(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}
