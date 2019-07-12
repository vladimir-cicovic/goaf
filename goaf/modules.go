package main

import (
	"fmt"
	"strings"
)

// Result holds the outcome of a module run on a single host.
type Result struct {
	Host    string
	Changed bool
	DryRun  bool   // true = would change (check mode), not applied
	Output  string
	Err     error
}

// Module defines the contract for all operations.
// Check reports whether a change is needed, without modifying state.
// Apply executes the change and returns output.
type Module interface {
	Name() string
	Check(s *Session) (bool, error)
	Apply(s *Session) (string, error)
}

// RunModule executes a module: checks state, applies only if needed.
// In checkMode, Apply is skipped and the result is marked as DryRun.
func RunModule(host string, s *Session, mod Module, checkMode bool) Result {
	needed, err := mod.Check(s)
	if err != nil {
		return Result{Host: host, Err: err}
	}
	if !needed {
		return Result{Host: host, Changed: false}
	}
	if checkMode {
		return Result{Host: host, Changed: true, DryRun: true, Output: "(would change)"}
	}
	out, err := mod.Apply(s)
	out = strings.TrimSpace(out)
	if err != nil {
		return Result{Host: host, Output: out, Err: err}
	}
	return Result{Host: host, Changed: true, Output: out}
}

// ---------- command module ----------
// Not idempotent — Check always returns true (always CHANGED).

type CommandModule struct {
	Cmd string
}

func (m CommandModule) Name() string                   { return "command" }
func (m CommandModule) Check(_ *Session) (bool, error) { return true, nil }

func (m CommandModule) Apply(s *Session) (string, error) {
	return s.Run(m.Cmd)
}

// ---------- package module ----------
// Idempotently installs a package: Check verifies, Apply installs.

type PackageModule struct {
	Pkg string
}

func (m PackageModule) Name() string { return "package" }

func (m PackageModule) Check(s *Session) (bool, error) {
	mgr := detectPkgMgr(s)
	if mgr == "" {
		return false, fmt.Errorf("no known package manager found (apt/dnf/yum)")
	}
	installed, err := isInstalled(s, mgr, m.Pkg)
	if err != nil {
		return false, fmt.Errorf("checking installation: %w", err)
	}
	return !installed, nil
}

func (m PackageModule) Apply(s *Session) (string, error) {
	mgr := detectPkgMgr(s)
	if mgr == "" {
		return "", fmt.Errorf("no known package manager found (apt/dnf/yum/apk/slackpkg/emerge/pacman/zypper)")
	}
	var cmd string
	switch mgr {
	case "apt":
		cmd = "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y " + m.Pkg
	case "dnf":
		cmd = "sudo dnf install -y " + m.Pkg
	case "yum":
		cmd = "sudo yum install -y " + m.Pkg
	case "apk":
		cmd = "sudo apk add --no-cache " + m.Pkg
	case "slackpkg":
		cmd = "sudo slackpkg -batch=on -default_answer=y install " + m.Pkg
	case "emerge":
		cmd = "sudo emerge --quiet --getbinpkg " + m.Pkg
	case "pacman":
		cmd = "sudo pacman -S --noconfirm " + m.Pkg
	case "zypper":
		cmd = "sudo zypper install -y " + m.Pkg
	}
	return s.Run(cmd)
}

func detectPkgMgr(s *Session) string {
	for _, mgr := range []string{"apt-get", "dnf", "yum", "apk", "slackpkg", "emerge", "pacman", "zypper"} {
		if out, _ := s.Run("which " + mgr + " 2>/dev/null"); strings.TrimSpace(out) != "" {
			if mgr == "apt-get" {
				return "apt"
			}
			return mgr
		}
	}
	return ""
}

func isInstalled(s *Session, mgr, pkg string) (bool, error) {
	var cmd string
	switch mgr {
	case "apt":
		cmd = "dpkg -s " + pkg + " >/dev/null 2>&1 && echo yes || echo no"
	case "dnf", "yum":
		// --whatprovides also catches virtual provides (e.g. wget2-wget provides wget)
		cmd = "rpm -q --whatprovides " + pkg + " >/dev/null 2>&1 && echo yes || echo no"
	case "apk":
		cmd = "apk info -e " + pkg + " >/dev/null 2>&1 && echo yes || echo no"
	case "slackpkg":
		// /var/log/packages/ contains files in format: pkgname-version-arch-build
		cmd = "ls /var/log/packages/ | grep -q '^" + pkg + "-' && echo yes || echo no"
	case "emerge":
		// /var/db/pkg/<category>/<pkgname>-<version>/ — search by package name only
		// pkg can be "app-text/tree" or just "tree"; take the part after the last "/"
		pkgName := pkg
		if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
			pkgName = pkg[idx+1:]
		}
		cmd = "find /var/db/pkg -maxdepth 2 -name '" + pkgName + "-[0-9]*' -type d | head -1 | grep -q . && echo yes || echo no"
	case "pacman":
		cmd = "pacman -Qi " + pkg + " >/dev/null 2>&1 && echo yes || echo no"
	case "zypper":
		// openSUSE is RPM-based — use the same rpm check as dnf
		cmd = "rpm -q --whatprovides " + pkg + " >/dev/null 2>&1 && echo yes || echo no"
	}
	out, err := s.Run(cmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}
