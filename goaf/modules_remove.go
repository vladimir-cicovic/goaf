package main

import (
	"fmt"
	"strings"
)

// RemoveModule idempotently removes a package.
// Check: verifies whether the package is installed.
// Apply: removes the package using the appropriate package manager command.
type RemoveModule struct {
	Pkg string
}

func (m RemoveModule) Name() string { return "remove" }

func (m RemoveModule) Check(s *Session) (bool, error) {
	mgr := detectPkgMgr(s)
	if mgr == "" {
		return false, fmt.Errorf("no known package manager found")
	}
	installed, err := isInstalled(s, mgr, m.Pkg)
	if err != nil {
		return false, fmt.Errorf("checking installation: %w", err)
	}
	return installed, nil // change needed only if the package is installed
}

func (m RemoveModule) Apply(s *Session) (string, error) {
	mgr := detectPkgMgr(s)
	if mgr == "" {
		return "", fmt.Errorf("no known package manager found")
	}
	var cmd string
	switch mgr {
	case "apt":
		cmd = "sudo DEBIAN_FRONTEND=noninteractive apt-get remove -y " + m.Pkg
	case "dnf":
		cmd = "sudo dnf remove -y " + m.Pkg
	case "yum":
		cmd = "sudo yum remove -y " + m.Pkg
	case "apk":
		cmd = "sudo apk del " + m.Pkg
	case "pacman":
		cmd = "sudo pacman -Rns --noconfirm " + m.Pkg
	case "zypper":
		cmd = "sudo zypper remove -y " + m.Pkg
	case "slackpkg":
		// removepkg is the standard Slackware removal tool
		pkgName := m.Pkg
		if idx := strings.LastIndex(m.Pkg, "/"); idx >= 0 {
			pkgName = m.Pkg[idx+1:]
		}
		cmd = "sudo removepkg " + pkgName
	case "emerge":
		// deselect removes from the world set, depclean actually uninstalls
		pkg := m.Pkg
		if !strings.Contains(pkg, "/") {
			pkg = m.Pkg
		}
		cmd = "sudo emerge --deselect " + pkg + " && sudo emerge --depclean --quiet"
	}
	return s.Run(cmd)
}
