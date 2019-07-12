package main

import (
	"fmt"
	"strings"
)

// ServiceModule manages service state (start/stop/restart) and boot status.
// Idempotency: checks current state before acting.
type ServiceModule struct {
	SvcName string
	State   string // "started" | "stopped" | "restarted"
	Enabled *bool  // nil = do not change boot status
}

func (m ServiceModule) Name() string { return "service" }

func (m ServiceModule) Check(s *Session) (bool, error) {
	if m.State == "restarted" {
		return true, nil // restart always changes state
	}

	init := detectInitSystem(s)

	isActive, err := serviceIsActive(s, init, m.SvcName)
	if err != nil {
		return false, err
	}

	needsChange := false
	switch m.State {
	case "started":
		needsChange = !isActive
	case "stopped":
		needsChange = isActive
	}

	if m.Enabled != nil && init == "systemd" {
		isEnabled, err := serviceIsEnabled(s, m.SvcName)
		if err == nil && *m.Enabled != isEnabled {
			needsChange = true
		}
	}

	return needsChange, nil
}

func (m ServiceModule) Apply(s *Session) (string, error) {
	init := detectInitSystem(s)
	if init == "" {
		return "", fmt.Errorf("no init system detected (systemd/openrc/sysv)")
	}

	var actions []string

	if m.State != "" {
		cmd, err := serviceStateCmd(init, m.SvcName, m.State)
		if err != nil {
			return "", err
		}
		if _, err := s.Run(cmd); err != nil {
			return "", fmt.Errorf("'%s %s': %w", m.State, m.SvcName, err)
		}
		actions = append(actions, m.State)
	}

	if m.Enabled != nil {
		if cmd := serviceEnabledCmd(init, m.SvcName, *m.Enabled); cmd != "" {
			if _, err := s.Run(cmd); err != nil {
				return "", fmt.Errorf("changing enabled status of '%s': %w", m.SvcName, err)
			}
			if *m.Enabled {
				actions = append(actions, "enabled")
			} else {
				actions = append(actions, "disabled")
			}
		}
	}

	return strings.Join(actions, "+") + ": " + m.SvcName, nil
}

func detectInitSystem(s *Session) string {
	// systemd: /run/systemd/system exists only when systemd is the active PID 1
	if out, _ := s.Run("test -d /run/systemd/system && echo yes"); strings.TrimSpace(out) == "yes" {
		return "systemd"
	}
	// OpenRC (Alpine etc.)
	if out, _ := s.Run("which rc-service 2>/dev/null"); strings.TrimSpace(out) != "" {
		return "openrc"
	}
	// SysV fallback
	if out, _ := s.Run("which service 2>/dev/null"); strings.TrimSpace(out) != "" {
		return "sysv"
	}
	return ""
}

func serviceIsActive(s *Session, init, name string) (bool, error) {
	var cmd string
	switch init {
	case "systemd":
		cmd = "systemctl is-active " + name + " 2>/dev/null"
	case "openrc":
		cmd = "rc-service " + name + " status 2>/dev/null | grep -q 'started' && echo active || echo inactive"
	case "sysv":
		cmd = "service " + name + " status >/dev/null 2>&1 && echo active || echo inactive"
	default:
		return false, fmt.Errorf("no init system detected")
	}
	out, _ := s.Run(cmd)
	return strings.TrimSpace(out) == "active", nil
}

func serviceIsEnabled(s *Session, name string) (bool, error) {
	out, _ := s.Run("systemctl is-enabled " + name + " 2>/dev/null")
	return strings.TrimSpace(out) == "enabled", nil
}

func serviceStateCmd(init, name, state string) (string, error) {
	action := map[string]string{
		"started":   "start",
		"stopped":   "stop",
		"restarted": "restart",
	}[state]
	if action == "" {
		return "", fmt.Errorf("unknown service state: %s", state)
	}
	switch init {
	case "systemd":
		return "systemctl " + action + " " + name, nil
	case "openrc":
		return "rc-service " + name + " " + action, nil
	case "sysv":
		return "service " + name + " " + action, nil
	}
	return "", fmt.Errorf("unknown init system: %s", init)
}

func serviceEnabledCmd(init, name string, enable bool) string {
	switch init {
	case "systemd":
		if enable {
			return "systemctl enable " + name
		}
		return "systemctl disable " + name
	case "openrc":
		if enable {
			return "rc-update add " + name
		}
		return "rc-update del " + name
	}
	return "" // sysv has no standard enable/disable command
}
