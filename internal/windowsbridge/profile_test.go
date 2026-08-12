package windowsbridge

import "testing"

func TestValidateProfileBoundary(t *testing.T) {
	module := `\\wsl.localhost\Ubuntu\home\agent\.cohotfs\bin\cohotfs-windows-bridge.exe`
	root := `\\wsl.localhost\Ubuntu\home\agent\.cohotfs\browser`
	profile := root + `\aaaaaaaaaaaaaaaaaaaaaaaaaa`
	if err := validateProfileBoundary(module, root, profile, "Ubuntu"); err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]struct {
		module, root, profile, distro string
	}{
		"wrong share":              {module, root, `\\wsl$\Ubuntu\home\agent\.cohotfs\browser\aaaaaaaaaaaaaaaaaaaaaaaaaa`, "Ubuntu"},
		"wrong distro":             {module, root, `\\wsl.localhost\Other\home\agent\.cohotfs\browser\aaaaaaaaaaaaaaaaaaaaaaaaaa`, "Ubuntu"},
		"profile prefix confusion": {module, root, root + `-copy\aaaaaaaaaaaaaaaaaaaaaaaaaa`, "Ubuntu"},
		"root prefix confusion":    {module, root + `-copy`, root + `-copy\aaaaaaaaaaaaaaaaaaaaaaaaaa`, "Ubuntu"},
		"nested profile":           {module, root, profile + `\nested`, "Ubuntu"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateProfileBoundary(candidate.module, candidate.root, candidate.profile, candidate.distro); err == nil {
				t.Fatal("unsafe Windows profile accepted")
			}
		})
	}
}
