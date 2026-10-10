package main

import (
	"fmt"
	"strings"
)

// FileModule creates/deletes a file or directory and sets permissions and ownership.
// Idempotency: compares existing attributes with the desired ones.
type FileModule struct {
	Path  string
	State string // "file" | "directory" | "absent"  (default: "file")
	Mode  string // e.g. "0644", "0755"
	Owner string // username
	Group string // group name
}

func (m FileModule) Name() string { return "file" }

func (m FileModule) Check(s Remote) (bool, error) {
	state := m.state()

	if isWinRM(s) {
		return m.checkWin(state, s)
	}
	out, _ := s.Run("stat -c '%F|%a|%U|%G' " + shQuote(m.Path) + " 2>/dev/null || echo ABSENT")
	current := strings.TrimSpace(out)

	if state == "absent" {
		return current != "ABSENT", nil
	}
	if current == "ABSENT" {
		return true, nil
	}

	parts := strings.SplitN(current, "|", 4)
	if len(parts) < 4 {
		return true, nil
	}
	fileType, currentMode, currentOwner, currentGroup := parts[0], parts[1], parts[2], parts[3]

	// stat can return "regular file" or "regular empty file" for plain files
	typeOK := false
	switch state {
	case "file":
		typeOK = strings.HasPrefix(fileType, "regular")
	case "directory":
		typeOK = fileType == "directory"
	}
	if !typeOK {
		return true, nil
	}
	if m.Mode != "" && currentMode != normalizeMode(m.Mode) {
		return true, nil
	}
	if m.Owner != "" && currentOwner != m.Owner {
		return true, nil
	}
	if m.Group != "" && currentGroup != m.Group {
		return true, nil
	}
	return false, nil
}

func (m FileModule) Apply(s Remote) (string, error) {
	state := m.state()

	if isWinRM(s) {
		return m.applyWin(state, s)
	}
	q := shQuote(m.Path)
	if state == "absent" {
		if _, err := s.Run("rm -rf " + q); err != nil {
			return "", fmt.Errorf("deleting '%s': %w", m.Path, err)
		}
		return "deleted: " + m.Path, nil
	}

	if state == "directory" {
		if _, err := s.Run("mkdir -p " + q); err != nil {
			return "", fmt.Errorf("creating directory '%s': %w", m.Path, err)
		}
	} else {
		if _, err := s.Run("mkdir -p $(dirname " + q + ") && touch " + q); err != nil {
			return "", fmt.Errorf("creating file '%s': %w", m.Path, err)
		}
	}

	if m.Mode != "" {
		if _, err := s.Run("chmod " + shQuote(m.Mode) + " " + q); err != nil {
			return "", fmt.Errorf("chmod '%s': %w", m.Path, err)
		}
	}

	if m.Owner != "" || m.Group != "" {
		var spec string
		if m.Owner != "" && m.Group != "" {
			spec = m.Owner + ":" + m.Group
		} else if m.Owner != "" {
			spec = m.Owner
		} else {
			spec = ":" + m.Group
		}
		if _, err := s.Run("chown " + shQuote(spec) + " " + q); err != nil {
			return "", fmt.Errorf("chown '%s': %w", m.Path, err)
		}
	}

	return state + ": " + m.Path, nil
}

func (m FileModule) state() string {
	if m.State == "" {
		return "file"
	}
	return m.State
}

// winStat returns "ABSENT", "file" or "directory" for Windows targets.
func winStat(s Remote, path string) string {
	out, _ := s.Run("if (Test-Path " + psQuote(path) + " -PathType Container) { 'directory' } elseif (Test-Path " + psQuote(path) + ") { 'file' } else { 'ABSENT' }")
	return strings.TrimSpace(out)
}

// checkWin is Check for Windows targets (type/state only;
// mode/owner/group are Unix concepts and ignored there).
func (m FileModule) checkWin(state string, s Remote) (bool, error) {
	current := winStat(s, m.Path)
	if state == "absent" {
		return current != "ABSENT", nil
	}
	if current == "ABSENT" {
		return true, nil
	}
	return current != state, nil
}

// applyWin is Apply for Windows targets (mode/owner/group ignored).
func (m FileModule) applyWin(state string, s Remote) (string, error) {
	q := psQuote(m.Path)
	if state == "absent" {
		if _, err := s.Run("Remove-Item -Path " + q + " -Recurse -Force"); err != nil {
			return "", fmt.Errorf("deleting '%s': %w", m.Path, err)
		}
		return "deleted: " + m.Path, nil
	}
	if state == "directory" {
		if _, err := s.Run("New-Item -Path " + q + " -ItemType Directory -Force | Out-Null"); err != nil {
			return "", fmt.Errorf("creating directory '%s': %w", m.Path, err)
		}
	} else {
		if _, err := s.Run("$d=[IO.Path]::GetDirectoryName(" + q + "); if ($d -and -not (Test-Path $d)) { New-Item -Path $d -ItemType Directory -Force | Out-Null }; New-Item -Path " + q + " -ItemType File -Force | Out-Null"); err != nil {
			return "", fmt.Errorf("creating file '%s': %w", m.Path, err)
		}
	}
	return state + ": " + m.Path, nil
}

// Diff shows which attributes would change (type/mode/owner/group).
func (m FileModule) Diff(s Remote) (string, error) {
	if isWinRM(s) {
		state := m.state()
		cur := winStat(s, m.Path)
		if cur == "ABSENT" {
			if state == "absent" {
				return "", nil
			}
			return "+" + state + ": " + m.Path, nil
		}
		if state == "absent" {
			return "-" + cur + ": " + m.Path, nil
		}
		if cur != state {
			return "-type=" + cur + "\n+type=" + state, nil
		}
		return "", nil
	}
	out, _ := s.Run("stat -c '%F|%a|%U|%G' " + shQuote(m.Path) + " 2>/dev/null || echo ABSENT")
	cur := strings.TrimSpace(out)
	state := m.state()
	if cur == "ABSENT" {
		if state == "absent" {
			return "", nil
		}
		return "+" + state + ": " + m.Path, nil
	}
	if state == "absent" {
		return "-" + cur + ": " + m.Path, nil
	}
	parts := strings.SplitN(cur, "|", 4)
	if len(parts) < 4 {
		return "", nil
	}
	wantType := map[string]string{"file": "regular", "directory": "directory"}[state]
	var b strings.Builder
	b.WriteString("--- " + m.Path + "\n+++ " + m.Path + " (desired)\n")
	changed := false
	attr := func(name, old, want string) {
		if want != "" && old != want {
			b.WriteString("-" + name + "=" + old + "\n+" + name + "=" + want + "\n")
			changed = true
		}
	}
	if wantType == "regular" {
		if !strings.HasPrefix(parts[0], "regular") {
			b.WriteString("-type=" + parts[0] + "\n+type=regular file\n")
			changed = true
		}
	} else if parts[0] != wantType {
		b.WriteString("-type=" + parts[0] + "\n+type=" + wantType + "\n")
		changed = true
	}
	attr("mode", parts[1], normalizeMode(m.Mode))
	attr("owner", parts[2], m.Owner)
	attr("group", parts[3], m.Group)
	if !changed {
		return "", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// normalizeMode strips leading zeros to match stat output (e.g. "0644" → "644").
func normalizeMode(mode string) string {
	mode = strings.TrimPrefix(mode, "0o")
	mode = strings.TrimLeft(mode, "0")
	if mode == "" {
		return "0"
	}
	return mode
}
