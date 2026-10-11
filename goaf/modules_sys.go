package main

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------- winget module (Windows only) ----------

type WingetModule struct {
	ID    string // package id
	State string // "present" (default) | "absent"
}

func (m WingetModule) Name() string { return "winget" }

func (m WingetModule) Check(s Remote) (bool, error) {
	if !isWinRM(s) {
		return false, fmt.Errorf("module 'winget' needs a Windows (WinRM) host")
	}
	out, _ := s.Run("winget list --id " + psQuote(m.ID) + " --accept-source-agreements | Select-String " + psQuote(m.ID))
	present := strings.Contains(out, m.ID)
	if m.State == "absent" {
		return present, nil
	}
	return !present, nil
}

func (m WingetModule) Apply(s Remote) (string, error) {
	if !isWinRM(s) {
		return "", fmt.Errorf("module 'winget' needs a Windows (WinRM) host")
	}
	state := m.State
	if state == "" {
		state = "present"
	}
	q := psQuote(m.ID)
	if state == "absent" {
		out, err := s.Run("winget uninstall --id " + q + " --silent --accept-source-agreements")
		out = strings.TrimSpace(out)
		if err != nil {
			return out, fmt.Errorf("uninstalling '%s': %w", m.ID, err)
		}
		return "uninstalled: " + m.ID, nil
	}
	out, err := s.Run("winget install --id " + q + " --silent --accept-package-agreements --accept-source-agreements --scope machine")
	out = strings.TrimSpace(out)
	if err != nil {
		return out, fmt.Errorf("installing '%s': %w", m.ID, err)
	}
	return "installed: " + m.ID, nil
}

// ---------- git module (any host with git) ----------

type GitModule struct {
	Repo    string
	Dest    string
	Version string // branch/tag/commit (default: default branch)
	Update  string // "true" (default) | "false"
}

func (m GitModule) Name() string { return "git" }

func (m GitModule) Check(s Remote) (bool, error) {
	out, _ := s.Run(gitTestCmd(m.Dest))
	if strings.TrimSpace(out) != "yes" {
		return true, nil // not cloned
	}
	if m.Version != "" {
		want, err := s.Run("git ls-remote " + shQuote(m.Repo) + " " + shQuote(m.Version) + " | head -1")
		if err != nil {
			return false, fmt.Errorf("resolving version '%s': %w", m.Version, err)
		}
		fields := strings.Fields(want)
		if len(fields) == 0 {
			return false, fmt.Errorf("version '%s' not found in %s", m.Version, m.Repo)
		}
		have, _ := s.Run("git -C " + shQuote(m.Dest) + " rev-parse HEAD")
		return strings.TrimSpace(have) != fields[0], nil
	}
	if m.Update == "false" {
		return false, nil
	}
	// Pull-check via upstream (no-op when no upstream configured).
	out, err := s.Run("git -C " + shQuote(m.Dest) + " fetch origin 2>&1 && git -C " + shQuote(m.Dest) + " rev-parse HEAD @{u} 2>/dev/null")
	if err != nil {
		return true, nil
	}
	fields := strings.Fields(out)
	return len(fields) < 2 || fields[0] != fields[1], nil
}

func gitTestCmd(dest string) string {
	return "git -C " + shQuote(dest) + " rev-parse --git-dir >/dev/null 2>&1 && echo yes || echo no"
}

func (m GitModule) Apply(s Remote) (string, error) {
	out, _ := s.Run(gitTestCmd(m.Dest))
	if strings.TrimSpace(out) != "yes" {
		cmd := "git clone "
		if m.Version != "" {
			cmd += "--branch " + shQuote(m.Version) + " "
		}
		cmd += shQuote(m.Repo) + " " + shQuote(m.Dest)
		res, err := s.Run(cmd)
		res = strings.TrimSpace(res)
		if err != nil {
			return res, fmt.Errorf("cloning %s: %w", m.Repo, err)
		}
		return "cloned " + m.Repo + " → " + m.Dest, nil
	}
	var cmd string
	if m.Version != "" {
		cmd = "git -C " + shQuote(m.Dest) + " fetch origin && git -C " + shQuote(m.Dest) + " checkout " + shQuote(m.Version)
	} else {
		cmd = "git -C " + shQuote(m.Dest) + " pull --ff-only"
	}
	res, err := s.Run(cmd)
	res = strings.TrimSpace(res)
	if err != nil {
		return res, fmt.Errorf("updating %s: %w", m.Dest, err)
	}
	return res, nil
}

// ---------- get_url module ----------

type GetURLModule struct {
	URL      string
	Dest     string
	Checksum string // "sha256:<hex>" (optional)
}

func (m GetURLModule) Name() string { return "get_url" }

func (m GetURLModule) Check(s Remote) (bool, error) {
	exists, _ := s.Run(remoteExistsCmd(s, m.Dest))
	if strings.TrimSpace(exists) != "yes" {
		return true, nil
	}
	if m.Checksum == "" {
		return false, nil
	}
	algo, hexSum, ok := splitChecksum(m.Checksum)
	if !ok {
		return false, fmt.Errorf("bad checksum %q, want sha256:<hex>", m.Checksum)
	}
	_ = algo
	var cmd string
	if isWinRM(s) {
		cmd = "(Get-FileHash -Algorithm SHA256 " + psQuote(m.Dest) + ").Hash"
	} else {
		cmd = "sha256sum " + shQuote(m.Dest) + " | awk '{print $1}'"
	}
	out, err := s.Run(cmd)
	if err != nil {
		return true, nil
	}
	return !strings.EqualFold(strings.TrimSpace(out), hexSum), nil
}

func splitChecksum(c string) (algo, hexSum string, ok bool) {
	parts := strings.SplitN(c, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// remoteExistsCmd answers "yes" when path exists (both transports).
func remoteExistsCmd(s Remote, path string) string {
	if isWinRM(s) {
		return "if (Test-Path " + psQuote(path) + ") { 'yes' } else { 'no' }"
	}
	return "test -e " + shQuote(path) + " && echo yes || echo no"
}

func (m GetURLModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		_, err := s.Run("$d=[IO.Path]::GetDirectoryName(" + psQuote(m.Dest) + "); if ($d -and -not (Test-Path $d)) { New-Item -Path $d -ItemType Directory -Force | Out-Null }; Invoke-WebRequest -Uri " + psQuote(m.URL) + " -OutFile " + psQuote(m.Dest))
		if err != nil {
			return "", fmt.Errorf("downloading %s: %w", m.URL, err)
		}
	} else {
		if _, err := s.Run("mkdir -p $(dirname " + shQuote(m.Dest) + ")"); err != nil {
			return "", fmt.Errorf("creating parent dir: %w", err)
		}
		out, err := s.Run("curl -fsSL -o " + shQuote(m.Dest) + " " + shQuote(m.URL) + " 2>&1 || wget -q -O " + shQuote(m.Dest) + " " + shQuote(m.URL) + " 2>&1")
		out = strings.TrimSpace(out)
		if err != nil {
			return out, fmt.Errorf("downloading %s (need curl or wget): %w", m.URL, err)
		}
	}
	if m.Checksum != "" {
		// Post-verify: re-check now that the file was (re)downloaded.
		needed, err := m.Check(s)
		if err != nil {
			return "", err
		}
		if needed {
			return "", fmt.Errorf("checksum mismatch for '%s' (want %s)", m.Dest, m.Checksum)
		}
	}
	return fmt.Sprintf("downloaded → %s", m.Dest), nil
}

// ---------- unarchive module ----------

type UnarchiveModule struct {
	Src     string // remote archive path
	Dest    string // target directory
	Creates string // sentinel file: skip when present (optional)
}

func (m UnarchiveModule) Name() string { return "unarchive" }

func (m UnarchiveModule) Check(s Remote) (bool, error) {
	if m.Creates != "" {
		out, _ := s.Run(remoteExistsCmd(s, m.Creates))
		return strings.TrimSpace(out) != "yes", nil
	}
	return true, nil // no sentinel: always unpack (like Ansible without creates)
}

func (m UnarchiveModule) Apply(s Remote) (string, error) {
	lower := strings.ToLower(m.Src)
	if isWinRM(s) {
		if _, err := s.Run("Expand-Archive -Path " + psQuote(m.Src) + " -DestinationPath " + psQuote(m.Dest) + " -Force"); err != nil {
			return "", fmt.Errorf("expanding '%s': %w", m.Src, err)
		}
		return fmt.Sprintf("expanded → %s", m.Dest), nil
	}
	if strings.HasSuffix(lower, ".zip") {
		if _, err := s.Run("mkdir -p " + shQuote(m.Dest) + " && unzip -o " + shQuote(m.Src) + " -d " + shQuote(m.Dest)); err != nil {
			return "", fmt.Errorf("expanding '%s' (need unzip): %w", m.Src, err)
		}
		return fmt.Sprintf("expanded → %s", m.Dest), nil
	}
	if _, err := s.Run("mkdir -p " + shQuote(m.Dest) + " && tar -xf " + shQuote(m.Src) + " -C " + shQuote(m.Dest)); err != nil {
		return "", fmt.Errorf("expanding '%s': %w", m.Src, err)
	}
	return fmt.Sprintf("expanded → %s", m.Dest), nil
}

// ---------- cron module (Unix) ----------

type CronModule struct {
	JobName string
	Job     string
	Minute  string
	Hour    string
	Dom     string
	Month   string
	Dow     string
	State   string // "present" (default) | "absent"
	User    string // crontab owner (needs privilege)
}

func (m CronModule) Name() string { return "cron" }

func (m CronModule) schedule() string {
	f := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	return f(m.Minute, "*") + " " + f(m.Hour, "*") + " " + f(m.Dom, "*") + " " + f(m.Month, "*") + " " + f(m.Dow, "*")
}

func (m CronModule) crontabCmd() string {
	if m.User != "" {
		return "crontab -u " + shQuote(m.User)
	}
	return "crontab"
}

func (m CronModule) block() string {
	return "# goaf:" + m.JobName + "\n" + m.schedule() + " " + m.Job + "\n"
}

func (m CronModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("module 'cron' is Unix-only (use schtasks via command on Windows)")
	}
	if m.Job == "" && m.State != "absent" {
		return false, fmt.Errorf("module 'cron' requires parameter 'job'")
	}
	out, _ := s.Run(m.crontabCmd() + " -l 2>/dev/null")
	want := ""
	state := m.State
	if state == "" {
		state = "present"
	}
	if state == "present" {
		want = m.block()
	}
	return extractCronBlock(out, m.JobName) != want, nil
}

// extractCronBlock returns the named "# goaf:name" block (marker + job
// lines) from a crontab, or "" when absent.
func extractCronBlock(tab, name string) string {
	lines := strings.Split(tab, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "# goaf:"+name && i+1 < len(lines) {
			return l + "\n" + lines[i+1] + "\n"
		}
	}
	return ""
}

func (m CronModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		return "", fmt.Errorf("module 'cron' is Unix-only (use schtasks via command on Windows)")
	}
	out, _ := s.Run(m.crontabCmd() + " -l 2>/dev/null")
	lines := strings.Split(out, "\n")
	var kept []string
	skipNext := false
	for _, l := range lines {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.TrimSpace(l) == "# goaf:"+m.JobName {
			skipNext = true // drop the job line after the marker
			continue
		}
		kept = append(kept, l)
	}
	state := m.State
	if state == "" {
		state = "present"
	}
	if state == "present" {
		kept = append(kept, "# goaf:"+m.JobName, m.schedule()+" "+m.Job)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	newTab := strings.Join(kept, "\n") + "\n"
	tmp := "/tmp/.goaf-crontab"
	if err := s.UploadContent([]byte(newTab), tmp); err != nil {
		return "", err
	}
	if _, err := s.Run(m.crontabCmd() + " " + shQuote(tmp) + " && rm -f " + shQuote(tmp)); err != nil {
		return "", fmt.Errorf("installing crontab: %w", err)
	}
	if state == "absent" {
		return "removed cron job: " + m.JobName, nil
	}
	return "installed cron job: " + m.JobName, nil
}

// ---------- sysctl module (Linux) ----------

type SysctlModule struct {
	Key     string
	Value   string
	State   string // "present" (default) | "absent"
	Persist string // sysctl.d file (default /etc/sysctl.d/99-goaf.conf)
}

func (m SysctlModule) Name() string { return "sysctl" }

func (m SysctlModule) persistFile() string {
	if m.Persist != "" {
		return m.Persist
	}
	return "/etc/sysctl.d/99-goaf.conf"
}

func (m SysctlModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("module 'sysctl' is Linux-only")
	}
	state := m.State
	if state == "" {
		state = "present"
	}
	if state == "absent" {
		lm := LineinfileModule{Path: m.persistFile(), Line: "", Regexp: "^" + regexpQuote(m.Key) + "\\s*="}
		_ = lm
		// Absent = persist entry missing (runtime value untouched).
		out, _ := s.Run("grep -q '^" + m.Key + "[[:space:]]*=' " + shQuote(m.persistFile()) + " 2>/dev/null && echo yes || echo no")
		return strings.TrimSpace(out) == "yes", nil
	}
	if m.Value == "" {
		return false, fmt.Errorf("module 'sysctl' requires parameter 'value'")
	}
	out, err := s.Run("sysctl -n " + shQuote(m.Key) + " 2>/dev/null")
	if err != nil || strings.TrimSpace(out) != m.Value {
		return true, nil
	}
	lm := LineinfileModule{Path: m.persistFile(), Line: m.Key + " = " + m.Value, Regexp: "^" + m.Key + "\\s*="}
	needed, err := lm.Check(s)
	if err != nil {
		return false, err
	}
	return needed, nil
}

// regexpQuote escapes a sysctl key for grep -E/basic usage.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (m SysctlModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		return "", fmt.Errorf("module 'sysctl' is Linux-only")
	}
	state := m.State
	if state == "" {
		state = "present"
	}
	lm := LineinfileModule{Path: m.persistFile(), Regexp: "^" + m.Key + "\\s*="}
	if state == "absent" {
		lm.Line = ""
		lm.State = "absent"
		if _, err := lm.Apply(s); err != nil {
			return "", err
		}
		return "removed sysctl persist: " + m.Key, nil
	}
	if _, err := s.Run("sysctl -w " + shQuote(m.Key+"="+m.Value)); err != nil {
		return "", fmt.Errorf("sysctl -w '%s': %w", m.Key, err)
	}
	lm.Line = m.Key + " = " + m.Value
	if _, err := lm.Apply(s); err != nil {
		return "", err
	}
	return fmt.Sprintf("sysctl %s = %s", m.Key, m.Value), nil
}

// ---------- mount module (Linux) ----------

type MountModule struct {
	Src    string // device
	Path   string // mount point
	Fstype string // e.g. ext4, nfs
	Opts   string // comma options (default "defaults")
	State  string // "mounted" (default) | "unmounted" | "present" | "absent"
}

func (m MountModule) Name() string { return "mount" }

func (m MountModule) mounted(s Remote) (bool, error) {
	out, err := s.Run("grep -q '[[:space:]]" + m.Path + "[[:space:]]' /proc/mounts && echo yes || echo no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

func (m MountModule) inFstab(s Remote) (bool, error) {
	out, _ := s.Run("grep -q '^" + m.Src + "[[:space:]]\\+" + m.Path + "[[:space:]]' /etc/fstab 2>/dev/null && echo yes || echo no")
	return strings.TrimSpace(out) == "yes", nil
}

func (m MountModule) fstabLine() string {
	opts := m.Opts
	if opts == "" {
		opts = "defaults"
	}
	fstype := m.Fstype
	if fstype == "" {
		fstype = "auto"
	}
	return m.Src + " " + m.Path + " " + fstype + " " + opts + " 0 0"
}

func (m MountModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("module 'mount' is Linux-only")
	}
	state := m.State
	if state == "" {
		state = "mounted"
	}
	switch state {
	case "mounted":
		ok, err := m.mounted(s)
		return !ok, err
	case "unmounted":
		ok, err := m.mounted(s)
		return ok, err
	case "present":
		ok, err := m.inFstab(s)
		return !ok, err
	case "absent":
		ok, err := m.inFstab(s)
		return ok, err
	}
	return false, fmt.Errorf("unknown mount state: %s", state)
}

func (m MountModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		return "", fmt.Errorf("module 'mount' is Linux-only")
	}
	state := m.State
	if state == "" {
		state = "mounted"
	}
	switch state {
	case "mounted":
		cmd := "mount "
		if m.Fstype != "" {
			cmd += "-t " + shQuote(m.Fstype) + " "
		}
		if m.Opts != "" {
			cmd += "-o " + shQuote(m.Opts) + " "
		}
		cmd += shQuote(m.Src) + " " + shQuote(m.Path)
		if _, err := s.Run(cmd); err != nil {
			return "", fmt.Errorf("mounting '%s': %w", m.Path, err)
		}
		return "mounted: " + m.Path, nil
	case "unmounted":
		if _, err := s.Run("umount " + shQuote(m.Path)); err != nil {
			return "", fmt.Errorf("unmounting '%s': %w", m.Path, err)
		}
		return "unmounted: " + m.Path, nil
	case "present":
		lm := LineinfileModule{Path: "/etc/fstab", Line: m.fstabLine(), Regexp: "^" + m.Src + "[[:space:]]\\+" + m.Path + "[[:space:]]"}
		if _, err := lm.Apply(s); err != nil {
			return "", err
		}
		return "fstab entry: " + m.Path, nil
	case "absent":
		lm := LineinfileModule{Path: "/etc/fstab", State: "absent", Regexp: "^" + m.Src + "[[:space:]]\\+" + m.Path + "[[:space:]]"}
		if _, err := lm.Apply(s); err != nil {
			return "", err
		}
		return "removed fstab entry: " + m.Path, nil
	}
	return "", fmt.Errorf("unknown mount state: %s", state)
}

// ---------- hostname module ----------

type HostnameModule struct {
	Host string
}

func (m HostnameModule) Name() string { return "hostname" }

func (m HostnameModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		out, _ := s.Run("$env:COMPUTERNAME")
		return !strings.EqualFold(strings.TrimSpace(out), m.Host), nil
	}
	out, err := s.Run("hostname 2>/dev/null || cat /etc/hostname 2>/dev/null")
	if err != nil {
		return false, fmt.Errorf("reading hostname: %w", err)
	}
	cur := strings.TrimSpace(strings.Split(out, ".")[0])
	want := strings.TrimSpace(strings.Split(m.Host, ".")[0])
	return !strings.EqualFold(cur, want), nil
}

func (m HostnameModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		if _, err := s.Run("Rename-Computer -NewName " + psQuote(m.Host) + " -Force"); err != nil {
			return "", fmt.Errorf("renaming computer: %w", err)
		}
		return "renamed (reboot required): " + m.Host, nil
	}
	if _, err := s.Run("hostnamectl set-hostname " + shQuote(m.Host) + " 2>/dev/null || (hostname " + shQuote(m.Host) + " && echo " + shQuote(m.Host) + " > /etc/hostname)"); err != nil {
		return "", fmt.Errorf("setting hostname: %w", err)
	}
	return "hostname: " + m.Host, nil
}

// ---------- timezone module ----------

type TimezoneModule struct {
	Zone string // Linux: Europe/Belgrade; Windows: Central European Standard Time
}

func (m TimezoneModule) Name() string { return "timezone" }

func (m TimezoneModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		out, _ := s.Run("(Get-TimeZone).Id")
		return strings.TrimSpace(out) != m.Zone, nil
	}
	out, err := s.Run("timedatectl show -p Timezone --value 2>/dev/null || readlink /etc/localtime 2>/dev/null")
	if err != nil {
		return false, fmt.Errorf("reading timezone: %w", err)
	}
	cur := strings.TrimSpace(out)
	if strings.Contains(cur, "/") && !strings.Contains(cur, "zoneinfo") {
		// timedatectl form already
	} else if idx := strings.Index(cur, "zoneinfo/"); idx >= 0 {
		cur = cur[idx+len("zoneinfo/"):]
	}
	return cur != m.Zone, nil
}

func (m TimezoneModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		if _, err := s.Run("Set-TimeZone -Id " + psQuote(m.Zone)); err != nil {
			return "", fmt.Errorf("setting timezone: %w", err)
		}
		return "timezone: " + m.Zone, nil
	}
	if _, err := s.Run("timedatectl set-timezone " + shQuote(m.Zone) + " 2>/dev/null || ln -sf /usr/share/zoneinfo/" + shQuote(m.Zone) + " /etc/localtime"); err != nil {
		return "", fmt.Errorf("setting timezone: %w", err)
	}
	return "timezone: " + m.Zone, nil
}

// ---------- stat module (info, never changes) ----------

type StatModule struct {
	Path string
}

func (m StatModule) Name() string   { return "stat" }
func (m StatModule) NoChange() bool { return true }

func (m StatModule) Check(_ Remote) (bool, error) { return true, nil }

func (m StatModule) Apply(s Remote) (string, error) {
	if isWinRM(s) {
		out, err := s.Run("$f=Get-Item " + psQuote(m.Path) + " -ErrorAction SilentlyContinue; if (-not $f) { 'exists=no' } else { 'exists=yes size=' + $f.Length + ' isdir=' + ($f.PSIsContainer) + ' mode=- owner=- group=- mtime=' + $f.LastWriteTimeUtc.ToString('o') }")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	}
	out, err := s.Run("if [ -e " + shQuote(m.Path) + " ]; then stat -c 'exists=yes size=%s mode=%a owner=%U group=%G mtime=%Y' " + shQuote(m.Path) + " 2>/dev/null; if [ -d " + shQuote(m.Path) + " ]; then echo 'isdir=yes'; else echo 'isdir=no'; fi; else echo 'exists=no'; fi")
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(out), " "), nil
}

// ---------- slurp module (file content into register, never changes) ----------

type SlurpModule struct {
	Src string
}

func (m SlurpModule) Name() string   { return "slurp" }
func (m SlurpModule) NoChange() bool { return true }

func (m SlurpModule) Check(_ Remote) (bool, error) { return true, nil }

func (m SlurpModule) Apply(s Remote) (string, error) {
	data, err := s.ReadRemote(m.Src)
	if err != nil {
		return "", fmt.Errorf("reading remote file: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ---------- find module (paths into register, never changes) ----------

type FindModule struct {
	Paths    string // directory (single)
	Patterns string // comma globs (default "*")
	FileType string // "file" (default) | "directory" | "any"
}

func (m FindModule) Name() string   { return "find" }
func (m FindModule) NoChange() bool { return true }

func (m FindModule) Check(_ Remote) (bool, error) { return true, nil }

func (m FindModule) Apply(s Remote) (string, error) {
	patterns := " *"
	if m.Patterns != "" {
		patterns = m.Patterns
	}
	if isWinRM(s) {
		var quoted []string
		for _, p := range strings.Split(patterns, ",") {
			if p = strings.TrimSpace(p); p != "" {
				quoted = append(quoted, psQuote(p))
			}
		}
		include := ""
		if len(quoted) > 0 {
			include = " -Include " + strings.Join(quoted, ",")
		}
		fileOnly := "-File"
		if m.FileType == "directory" {
			fileOnly = "-Directory"
		} else if m.FileType == "any" {
			fileOnly = ""
		}
		// -Include needs a wildcard path (or -Recurse).
		out, err := s.Run("Get-ChildItem -Path " + psQuote(m.Paths+"\\*") + include + " " + fileOnly + " -ErrorAction SilentlyContinue | Select-Object -ExpandProperty FullName")
		if err != nil {
			return "", err
		}
		out = strings.TrimSpace(out)
		if out == "" {
			return "no matches", nil
		}
		return out, nil
	}
	findType := "-type f"
	if m.FileType == "directory" {
		findType = "-type d"
	} else if m.FileType == "any" {
		findType = ""
	}
	var names []string
	for _, p := range strings.Split(patterns, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		names = append(names, "-name "+shQuote(p))
	}
	nameExpr := strings.Join(names, " -o ")
	if len(names) > 1 {
		nameExpr = "\\( " + nameExpr + " \\)"
	}
	if nameExpr == "" {
		nameExpr = ""
	}
	cmd := "find " + shQuote(m.Paths) + " -maxdepth 1 " + findType
	if nameExpr != "" {
		cmd += " " + nameExpr
	}
	out, err := s.Run(cmd + " 2>/dev/null | sort")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "no matches", nil
	}
	return out, nil
}

// ---------- wait_for module (never changes, fails on timeout) ----------

type WaitForModule struct {
	Port    string // TCP port to wait for
	Host    string // host of the port (default 127.0.0.1)
	Path    string // file path to wait for
	Timeout string // seconds (default 300)
	Delay   string // seconds before first check (default 0)
}

func (m WaitForModule) Name() string   { return "wait_for" }
func (m WaitForModule) NoChange() bool { return true }

func (m WaitForModule) Check(_ Remote) (bool, error) { return true, nil }

func (m WaitForModule) Apply(s Remote) (string, error) {
	timeout := atoiOr(m.Timeout, 300)
	delay := atoiOr(m.Delay, 0)
	if m.Port == "" && m.Path == "" {
		return "", fmt.Errorf("wait_for needs 'port' or 'path'")
	}
	check := func() (bool, error) { return false, nil }
	host := m.Host
	if host == "" {
		host = "127.0.0.1"
	}
	if m.Port != "" {
		if isWinRM(s) {
			check = func() (bool, error) {
				out, err := s.Run("(Test-NetConnection -ComputerName " + psQuote(host) + " -Port " + m.Port + " -WarningAction SilentlyContinue).TcpTestSucceeded")
				if err != nil {
					return false, nil
				}
				return strings.TrimSpace(out) == "True", nil
			}
		} else {
			check = func() (bool, error) {
				out, err := s.Run("bash -c '</dev/tcp/" + host + "/" + m.Port + "' >/dev/null 2>&1 && echo open || (command -v nc >/dev/null 2>&1 && nc -z -w2 " + shQuote(host) + " " + shQuote(m.Port) + " >/dev/null 2>&1 && echo open || echo closed)")
				if err != nil {
					return false, nil
				}
				return strings.TrimSpace(out) == "open", nil
			}
		}
	} else {
		check = func() (bool, error) {
			out, err := s.Run(remoteExistsCmd(s, m.Path))
			if err != nil {
				return false, nil
			}
			return strings.TrimSpace(out) == "yes", nil
		}
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	if delay > 0 {
		time.Sleep(time.Duration(delay) * time.Second)
	}
	for {
		ok, _ := check()
		if ok {
			return "condition met", nil
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("wait_for timed out after %ds", timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

// ---------- pause module (control-side sleep, never changes) ----------

type PauseModule struct {
	Seconds string
	Prompt  string
}

func (m PauseModule) Name() string   { return "pause" }
func (m PauseModule) NoChange() bool { return true }

func (m PauseModule) Check(_ Remote) (bool, error) { return true, nil }

func (m PauseModule) Apply(_ Remote) (string, error) {
	n := atoiOr(m.Seconds, 0)
	if m.Prompt != "" {
		fmt.Printf("PAUSE: %s\n", m.Prompt)
	}
	if n > 0 {
		time.Sleep(time.Duration(n) * time.Second)
		return fmt.Sprintf("paused %ds", n), nil
	}
	return "pause (no wait)", nil
}

// atoiOr parses an int, returning def on empty/garbage.
func atoiOr(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
