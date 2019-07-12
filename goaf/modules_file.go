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

func (m FileModule) Check(s *Session) (bool, error) {
	state := m.state()

	out, _ := s.Run("stat -c '%F|%a|%U|%G' " + m.Path + " 2>/dev/null || echo ABSENT")
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

func (m FileModule) Apply(s *Session) (string, error) {
	state := m.state()

	if state == "absent" {
		if _, err := s.Run("rm -rf " + m.Path); err != nil {
			return "", fmt.Errorf("deleting '%s': %w", m.Path, err)
		}
		return "deleted: " + m.Path, nil
	}

	if state == "directory" {
		if _, err := s.Run("mkdir -p " + m.Path); err != nil {
			return "", fmt.Errorf("creating directory '%s': %w", m.Path, err)
		}
	} else {
		if _, err := s.Run("mkdir -p $(dirname " + m.Path + ") && touch " + m.Path); err != nil {
			return "", fmt.Errorf("creating file '%s': %w", m.Path, err)
		}
	}

	if m.Mode != "" {
		if _, err := s.Run("chmod " + m.Mode + " " + m.Path); err != nil {
			return "", fmt.Errorf("chmod '%s': %w", m.Path, err)
		}
	}

	if m.Owner != "" || m.Group != "" {
		var chown string
		if m.Owner != "" && m.Group != "" {
			chown = m.Owner + ":" + m.Group
		} else if m.Owner != "" {
			chown = m.Owner
		} else {
			chown = ":" + m.Group
		}
		if _, err := s.Run("chown " + chown + " " + m.Path); err != nil {
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

// normalizeMode strips leading zeros to match stat output (e.g. "0644" → "644").
func normalizeMode(mode string) string {
	mode = strings.TrimPrefix(mode, "0o")
	mode = strings.TrimLeft(mode, "0")
	if mode == "" {
		return "0"
	}
	return mode
}
