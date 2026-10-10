package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// waitForReboot polls until SSH is reachable AND the kernel boot_id differs
// from oldBootID (proves the reboot actually happened), or the timeout
// in seconds expires. When oldBootID is empty, any reachable SSH counts.
func waitForReboot(s *Session, timeoutSec int, oldBootID string) error {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	addr := fmt.Sprintf("%s:%d", s.addr, s.port)
	for time.Now().Before(deadline) {
		if methods, err := authMethods(s.keyPath); err == nil {
			cfg := &ssh.ClientConfig{
				User:            s.user,
				Auth:            methods,
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				Timeout:         5 * time.Second,
			}
			if c, derr := ssh.Dial("tcp", addr, cfg); derr == nil {
				bootID := ""
				if sess, serr := c.NewSession(); serr == nil {
					if out, oerr := sess.CombinedOutput("cat /proc/sys/kernel/random/boot_id 2>/dev/null"); oerr == nil {
						bootID = strings.TrimSpace(string(out))
					}
					sess.Close()
				}
				c.Close()
				if oldBootID == "" || (bootID != "" && bootID != oldBootID) {
					return nil
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("host did not come back with a new boot within %ds on %s", timeoutSec, addr)
}

// ---------- debug / set_fact modules ----------
// Both are control-side: the playbook runner intercepts them before any SSH
// (debug prints a variable/message, set_fact merges into host vars).
// The stubs below only exist so ad-hoc mode and validate accept the names.

type DebugModule struct {
	Var string
	Msg string
}

func (m DebugModule) Name() string                   { return "debug" }
func (m DebugModule) Check(_ Remote) (bool, error)   { return false, nil }
func (m DebugModule) Apply(_ Remote) (string, error) { return "", nil }

type SetFactModule struct {
	Vars map[string]string
}

func (m SetFactModule) Name() string                   { return "set_fact" }
func (m SetFactModule) Check(_ Remote) (bool, error)   { return false, nil }
func (m SetFactModule) Apply(_ Remote) (string, error) { return "", nil }

// ---------- meta pseudo-module ----------
// Meta tasks (flush_handlers) are control-side: the runner intercepts them.
// This stub only exists so ad-hoc mode fails with a clear message instead
// of "unknown module".

type MetaModule struct{}

func (m MetaModule) Name() string { return "meta" }
func (m MetaModule) Check(_ Remote) (bool, error) {
	return false, fmt.Errorf("meta tasks are playbook-only (flush_handlers)")
}
func (m MetaModule) Apply(_ Remote) (string, error) { return "", nil }

// ---------- user module ----------
// Manages local user accounts (needs privilege: sudo is used internally,
// like the package modules).

type UserModule struct {
	Username string
	State    string // "present" (default) | "absent"
	Shell    string // login shell for new users
	Groups   string // comma-separated supplementary groups
}

func (m UserModule) Name() string { return "user" }

func (m UserModule) Check(s Remote) (bool, error) {
	state := m.State
	if state == "" {
		state = "present"
	}
	if isWinRM(s) {
		out, _ := s.Run("(Get-LocalUser -Name " + psQuote(m.Username) + " -ErrorAction SilentlyContinue) -ne $null")
		exists := strings.TrimSpace(out) == "True"
		if state == "absent" {
			return exists, nil
		}
		return !exists, nil
	}
	out, _ := s.Run("id " + shQuote(m.Username) + " >/dev/null 2>&1 && echo yes || echo no")
	exists := strings.TrimSpace(out) == "yes"
	if state == "absent" {
		return exists, nil
	}
	return !exists, nil
}

func (m UserModule) Apply(s Remote) (string, error) {
	state := m.State
	if state == "" {
		state = "present"
	}
	if isWinRM(s) {
		return m.applyWin(s)
	}
	if state == "absent" {
		if _, err := s.Run("sudo userdel -r " + shQuote(m.Username)); err != nil {
			return "", fmt.Errorf("deleting user '%s': %w", m.Username, err)
		}
		return "deleted user: " + m.Username, nil
	}
	cmd := "sudo useradd -m"
	if m.Shell != "" {
		cmd += " -s " + shQuote(m.Shell)
	}
	if m.Groups != "" {
		cmd += " -G " + shQuote(m.Groups)
	}
	cmd += " " + shQuote(m.Username)
	if _, err := s.Run(cmd); err != nil {
		return "", fmt.Errorf("creating user '%s': %w", m.Username, err)
	}
	return "created user: " + m.Username, nil
}

// applyWin manages local Windows users (shell/groups are Unix-only, ignored).
func (m UserModule) applyWin(s Remote) (string, error) {
	state := m.State
	if state == "" {
		state = "present"
	}
	q := psQuote(m.Username)
	if state == "absent" {
		if _, err := s.Run("Remove-LocalUser -Name " + q); err != nil {
			return "", fmt.Errorf("deleting user '%s': %w", m.Username, err)
		}
		return "deleted user: " + m.Username, nil
	}
	cmd := "New-LocalUser -Name " + q + " -NoPassword"
	if _, err := s.Run(cmd); err != nil {
		return "", fmt.Errorf("creating user '%s': %w", m.Username, err)
	}
	for _, g := range strings.Split(m.Groups, ",") {
		if g = strings.TrimSpace(g); g != "" {
			if _, err := s.Run("Add-LocalGroupMember -Group " + psQuote(g) + " -Member " + q); err != nil {
				return "", fmt.Errorf("adding '%s' to group '%s': %w", m.Username, g, err)
			}
		}
	}
	return "created user: " + m.Username, nil
}

// ---------- lineinfile module ----------
// Ensures a single line exists in (or is absent from) a remote text file.

type LineinfileModule struct {
	Path   string
	Line   string
	Regexp string // Go regexp; match is replaced, else line is appended
	State  string // "present" (default) | "absent"
}

func (m LineinfileModule) Name() string { return "lineinfile" }

func (m LineinfileModule) desired(remote string) (string, bool, error) {
	state := m.State
	if state == "" {
		state = "present"
	}
	lines := splitDiffLines(remote)
	if state == "absent" {
		kept, removed := []string{}, false
		for _, l := range lines {
			if m.matches(l) {
				removed = true
				continue
			}
			kept = append(kept, l)
		}
		if !removed {
			return remote, false, nil
		}
		return strings.Join(kept, "\n") + "\n", true, nil
	}
	if m.Regexp != "" {
		re, err := regexp.Compile(m.Regexp)
		if err != nil {
			return "", false, fmt.Errorf("bad regexp: %w", err)
		}
		matched, changed := false, false
		for i, l := range lines {
			if re.MatchString(l) {
				matched = true
				if l != m.Line {
					lines[i] = m.Line
					changed = true
				}
			}
		}
		if !matched {
			lines = append(lines, m.Line)
			changed = true
		}
		if !changed {
			return remote, false, nil
		}
		return strings.Join(lines, "\n") + "\n", true, nil
	}
	for _, l := range lines {
		if l == m.Line {
			return remote, false, nil
		}
	}
	out := remote
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + m.Line + "\n", true, nil
}

func (m LineinfileModule) matches(l string) bool {
	if m.Regexp != "" {
		ok, _ := regexp.MatchString(m.Regexp, l)
		return ok
	}
	return l == m.Line
}

func (m LineinfileModule) Check(s Remote) (bool, error) {
	remote, _ := s.ReadRemote(m.Path) // missing file → treated as empty
	_, needed, err := m.desired(string(remote))
	return needed, err
}

func (m LineinfileModule) Apply(s Remote) (string, error) {
	remote, _ := s.ReadRemote(m.Path)
	want, needed, err := m.desired(string(remote))
	if err != nil {
		return "", err
	}
	if !needed {
		return "line already present: " + m.Path, nil
	}
	if err := s.UploadContent([]byte(want), m.Path); err != nil {
		return "", err
	}
	return fmt.Sprintf("updated → %s", m.Path), nil
}

// Diff shows the unified diff of current vs desired file content.
func (m LineinfileModule) Diff(s Remote) (string, error) {
	remote, _ := s.ReadRemote(m.Path)
	want, _, err := m.desired(string(remote))
	if err != nil {
		return "", err
	}
	return unifiedDiff(m.Path, m.Path, string(remote), want, 3), nil
}

// ---------- script module ----------
// Uploads a local script to the host and executes it (always runs,
// like the command module). Linux-only (executed with sh).

type ScriptModule struct {
	Src  string // local script path
	Args string // arguments appended after the script path
}

func (m ScriptModule) Name() string { return "script" }

func (m ScriptModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("module 'script' is not supported over WinRM (POSIX shell only)")
	}
	return true, nil
}

func (m ScriptModule) Apply(s Remote) (string, error) {
	remote := "/tmp/.goaf-script-" + fmt.Sprintf("%d", time.Now().UnixNano()) + ".sh"
	if err := s.Upload(m.Src, remote); err != nil {
		return "", err
	}
	// Best-effort cleanup; the run result matters, not the rm.
	defer func() {
		_, _ = s.Run("rm -f " + shQuote(remote))
	}()
	if _, err := s.Run("chmod +x " + shQuote(remote)); err != nil {
		return "", fmt.Errorf("chmod script: %w", err)
	}
	cmd := "sh " + shQuote(remote)
	if m.Args != "" {
		cmd += " " + m.Args
	}
	out, err := s.Run(cmd)
	out = strings.TrimSpace(out)
	if err != nil {
		return out, err
	}
	return out, nil
}

// ---------- fetch module ----------
// Downloads a remote file to the control node (reverse of copy).

type FetchModule struct {
	Src  string // remote path
	Dest string // local path; when an existing directory, keeps the file name
}

func (m FetchModule) Name() string { return "fetch" }

func (m FetchModule) Check(s Remote) (bool, error) {
	local, err := m.localPath()
	if err != nil {
		return false, err
	}
	want, err := os.ReadFile(local)
	if err != nil {
		return true, nil // missing locally → download
	}
	remote, err := s.ReadRemote(m.Src)
	if err != nil {
		return false, fmt.Errorf("reading remote file: %w", err)
	}
	return string(remote) != string(want), nil
}

func (m FetchModule) Apply(s Remote) (string, error) {
	local, err := m.localPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return "", err
	}
	remote, err := s.ReadRemote(m.Src)
	if err != nil {
		return "", fmt.Errorf("reading remote file: %w", err)
	}
	if err := os.WriteFile(local, remote, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("fetched → %s", local), nil
}

// localPath resolves Dest to a file path: an existing directory or a
// trailing separator means "keep the remote file name inside Dest".
func (m FetchModule) localPath() (string, error) {
	if m.Dest == "" {
		return "", fmt.Errorf("module 'fetch' requires parameter 'dest'")
	}
	if st, err := os.Stat(m.Dest); err == nil && st.IsDir() {
		return filepath.Join(m.Dest, path.Base(m.Src)), nil
	}
	if strings.HasSuffix(m.Dest, "/") || strings.HasSuffix(m.Dest, string(os.PathSeparator)) {
		return filepath.Join(m.Dest, path.Base(m.Src)), nil
	}
	return m.Dest, nil
}

// ---------- authorized_key module ----------
// Manages one public key in a user's ~/.ssh/authorized_keys.

type AuthorizedKeyModule struct {
	User  string
	Key   string // full public key line ("ssh-ed25519 AAAA... comment")
	State string // "present" (default) | "absent"
}

func (m AuthorizedKeyModule) Name() string { return "authorized_key" }

func (m AuthorizedKeyModule) homeDir(s Remote) (string, error) {
	out, err := s.Run("getent passwd " + shQuote(m.User) + " | cut -d: -f6")
	if err != nil {
		return "", fmt.Errorf("resolving home of user '%s': %w", m.User, err)
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", fmt.Errorf("unknown user '%s'", m.User)
	}
	return home, nil
}

func (m AuthorizedKeyModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("module 'authorized_key' is not supported over WinRM (Linux SSH keys only)")
	}
	home, err := m.homeDir(s)
	if err != nil {
		return false, err
	}
	state := m.State
	if state == "" {
		state = "present"
	}
	out, _ := s.Run("grep -qxF " + shQuote(m.Key) + " " + shQuote(home+"/.ssh/authorized_keys") + " 2>/dev/null && echo yes || echo no")
	present := strings.TrimSpace(out) == "yes"
	if state == "absent" {
		return present, nil
	}
	return !present, nil
}

func (m AuthorizedKeyModule) Apply(s Remote) (string, error) {
	home, err := m.homeDir(s)
	if err != nil {
		return "", err
	}
	state := m.State
	if state == "" {
		state = "present"
	}
	ak := home + "/.ssh/authorized_keys"
	qak := shQuote(ak)
	qkey := shQuote(m.Key)
	// File operations run under sudo like the user/package modules:
	// the target home usually belongs to another user.
	if state == "absent" {
		cmd := "sudo test -f " + qak + " && sudo grep -vxF " + qkey + " " + qak + " | sudo tee " + qak + ".goaf-tmp >/dev/null && sudo cat " + qak + ".goaf-tmp | sudo tee " + qak + " >/dev/null && sudo rm -f " + qak + ".goaf-tmp || true"
		if _, err := s.Run(cmd); err != nil {
			return "", fmt.Errorf("removing key for '%s': %w", m.User, err)
		}
		return "removed key for user: " + m.User, nil
	}
	// (grep || echo) is grouped so a missing key still continues the chain.
	cmds := "sudo mkdir -p " + shQuote(home+"/.ssh") +
		" && sudo chmod 700 " + shQuote(home+"/.ssh") +
		" && sudo touch " + qak +
		" && (sudo grep -qxF " + qkey + " " + qak + " || echo " + qkey + " | sudo tee -a " + qak + " >/dev/null)" +
		" && sudo chmod 600 " + qak +
		" && sudo chown " + shQuote(m.User) + " " + shQuote(home+"/.ssh") + " " + qak
	if _, err := s.Run(cmds); err != nil {
		return "", fmt.Errorf("adding key for '%s': %w", m.User, err)
	}
	return "added key for user: " + m.User, nil
}

// ---------- reboot module ----------
// Reboots the host and waits until SSH is reachable again.

type RebootModule struct {
	Timeout int    // max seconds to wait for SSH return (default 300)
	Msg     string // informational only, shown in output
}

func (m RebootModule) Name() string { return "reboot" }

func (m RebootModule) timeout() int {
	if m.Timeout <= 0 {
		return 300
	}
	return m.Timeout
}

func (m RebootModule) Check(_ Remote) (bool, error) { return true, nil }

func (m RebootModule) Apply(s Remote) (string, error) {
	if w, ok := s.(*WinRMSession); ok {
		return m.applyWin(w)
	}
	ss, ok := s.(*Session)
	if !ok {
		return "", fmt.Errorf("reboot: unsupported session type")
	}
	// Fire-and-forget: the connection will drop. Needs privilege like
	// the package modules (sudo is embedded, become wraps harmlessly).
	oldID, _ := s.Run("cat /proc/sys/kernel/random/boot_id 2>/dev/null")
	oldID = strings.TrimSpace(oldID)
	_, _ = s.Run("sudo -n sh -c 'sleep 2; reboot' >/dev/null 2>&1 & echo rebooting")
	if err := waitForReboot(ss, m.timeout(), oldID); err != nil {
		return "", err
	}
	if m.Msg != "" {
		return "rebooted: " + m.Msg, nil
	}
	return "rebooted, SSH reachable again", nil
}

// applyWin reboots a Windows host and waits for WinRM to return.
func (m RebootModule) applyWin(s *WinRMSession) (string, error) {
	oldBoot, _ := s.Run("(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToString('o')")
	_, _ = s.Run("Restart-Computer -Force")
	if err := waitForRebootWinRM(s, m.timeout(), strings.TrimSpace(oldBoot)); err != nil {
		return "", err
	}
	if m.Msg != "" {
		return "rebooted: " + m.Msg, nil
	}
	return "rebooted, WinRM reachable again", nil
}

// ---------- upgrade module ----------
// Upgrades all packages with the detected package manager (always runs,
// like the command module).

type UpgradeModule struct{}

func (m UpgradeModule) Name() string { return "upgrade" }

func (m UpgradeModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("module 'upgrade' is not supported over WinRM (Linux package managers only)")
	}
	return true, nil
}

func (m UpgradeModule) Apply(s Remote) (string, error) {
	mgr := detectPkgMgr(s)
	if mgr == "" {
		return "", fmt.Errorf("no known package manager found (apt/dnf/yum/apk/slackpkg/emerge/pacman/zypper)")
	}
	var cmd string
	switch mgr {
	case "apt":
		cmd = "sudo apt-get update -q && sudo DEBIAN_FRONTEND=noninteractive apt-get upgrade -y"
	case "dnf":
		cmd = "sudo dnf upgrade -y"
	case "yum":
		cmd = "sudo yum update -y"
	case "apk":
		cmd = "sudo apk upgrade"
	case "slackpkg":
		cmd = "sudo slackpkg update && sudo slackpkg -batch=on -default_answer=y upgrade-all"
	case "emerge":
		cmd = "sudo emerge --update --deep --newuse @world"
	case "pacman":
		cmd = "sudo pacman -Syu --noconfirm"
	case "zypper":
		cmd = "sudo zypper update -y"
	}
	out, err := s.Run(cmd)
	out = strings.TrimSpace(out)
	if err != nil {
		return out, fmt.Errorf("upgrade: %w", err)
	}
	return out, nil
}
