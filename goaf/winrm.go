package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/masterzen/winrm"
)

// winrmChunk is the max base64 characters per upload command (well under
// WinRM envelope limits).
const winrmChunk = 4000

// WinRMSession manages one Windows host over WinRM (HTTP + NTLM).
// Shell is PowerShell, run via RunPSWithString (the library encodes transport).
// Become is accepted but ignored: Windows has no sudo (documented).
type WinRMSession struct {
	Host     string
	Become   bool
	client   *winrm.Client
	user     string
	password string
}

// DisplayHost implements Remote.
func (s *WinRMSession) DisplayHost() string { return s.Host }

// SetBecome implements Remote (accepted, ignored on Windows).
func (s *WinRMSession) SetBecome(bool) {}

// psQuote quotes a string for PowerShell single-quoted literals.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// winParent returns the parent directory of a Windows path (both / and \).
func winParent(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i > 0 {
		// Keep drive roots ("C:") intact: parent of C:/x is C:/, not C:.
		if p[i-1] == ':' {
			return p[:i+1]
		}
		return p[:i]
	}
	return ""
}

// ConnectWinRM dials a Windows host (validates with a no-op probe).
func ConnectWinRM(h Host, password string) (*WinRMSession, error) {
	label := hostLabel(h)
	port := h.Port
	if port == 0 {
		port = 5985
	}
	endpoint := winrm.NewEndpoint(h.Addr, port, false, false, nil, nil, nil, 0)
	params := winrm.NewParameters("PT60S", "en-US", 153600)
	params.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	client, err := winrm.NewClientWithParameters(endpoint, h.User, password, params)
	if err != nil {
		return nil, err
	}
	s := &WinRMSession{Host: label, client: client, user: h.User, password: password}
	if _, err := s.Run("$null"); err != nil {
		return nil, fmt.Errorf("WinRM probe failed on %s: %w", label, err)
	}
	return s, nil
}

// Run executes PowerShell code, returning trimmed combined output.
// Non-zero exit is an error (SSH-like semantics).
func (s *WinRMSession) Run(cmd string) (string, error) {
	stdout, stderr, code, err := s.client.RunPSWithString(cmd, "")
	out := strings.TrimSpace(stdout)
	if strings.TrimSpace(stderr) != "" {
		if out != "" {
			out += "\n"
		}
		out += strings.TrimSpace(stderr)
	}
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("process exited with status %d", code)
	}
	return out, nil
}

// Upload copies a local file to the remote host via chunked base64.
func (s *WinRMSession) Upload(localPath, remotePath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("opening local file: %w", err)
	}
	return s.UploadContent(data, remotePath)
}

// UploadContent writes bytes to the remote host via chunked base64.
func (s *WinRMSession) UploadContent(content []byte, remotePath string) error {
	parent := winParent(remotePath)
	if parent != "" {
		if _, err := s.Run("New-Item -Path " + psQuote(parent) + " -ItemType Directory -Force | Out-Null"); err != nil {
			return fmt.Errorf("creating parent directory: %w", err)
		}
	}
	b64 := base64.StdEncoding.EncodeToString(content)
	tmp := remotePath + ".goaf-b64"
	first := true
	for len(b64) > 0 {
		n := winrmChunk
		if n > len(b64) {
			n = len(b64)
		}
		chunk, rest := b64[:n], b64[n:]
		b64 = rest
		var cmd string
		if first {
			cmd = "[IO.File]::WriteAllText(" + psQuote(tmp) + ", " + psQuote(chunk) + ")"
			first = false
		} else {
			cmd = "Add-Content -Path " + psQuote(tmp) + " -Value " + psQuote(chunk) + " -NoNewline"
		}
		if _, err := s.Run(cmd); err != nil {
			return fmt.Errorf("uploading chunk: %w", err)
		}
	}
	decode := "$b=[Convert]::FromBase64String([IO.File]::ReadAllText(" + psQuote(tmp) + ")); " +
		"[IO.File]::WriteAllBytes(" + psQuote(remotePath) + ", $b); " +
		"Remove-Item " + psQuote(tmp) + " -Force"
	if _, err := s.Run(decode); err != nil {
		return fmt.Errorf("decoding upload: %w", err)
	}
	return nil
}

// ReadRemote downloads a remote file (base64 round-trip).
func (s *WinRMSession) ReadRemote(remotePath string) ([]byte, error) {
	out, err := s.Run("[Convert]::ToBase64String([IO.File]::ReadAllBytes(" + psQuote(remotePath) + "))")
	if err != nil {
		return nil, fmt.Errorf("reading remote file: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("decoding remote file: %w", err)
	}
	return raw, nil
}

// Close implements Remote (WinRM is request-based; nothing persistent).
func (s *WinRMSession) Close() {}

// winrmPwCache holds a prompted password so the user is asked only once.
var winrmPwCache string

// winrmAskPass is set from --ask-winrm-pass.
var winrmAskPass bool

// winrmPassword resolves the WinRM password: inventory, GOAF_WINRM_PASSWORD,
// or an interactive prompt (with --ask-winrm-pass).
func winrmPassword(h Host) (string, error) {
	if h.Password != "" {
		return h.Password, nil
	}
	if winrmPwCache != "" {
		return winrmPwCache, nil
	}
	if pw := os.Getenv("GOAF_WINRM_PASSWORD"); pw != "" {
		return pw, nil
	}
	if winrmAskPass {
		pw, err := readPassword("WINRM password: ")
		if err != nil {
			return "", err
		}
		winrmPwCache = pw
		return pw, nil
	}
	return "", fmt.Errorf("no WinRM password for %s (inventory password:, --ask-winrm-pass or GOAF_WINRM_PASSWORD)", hostLabel(h))
}

// dialHost connects via SSH or WinRM depending on the host config.
func dialHost(h Host) (Remote, error) {
	if strings.ToLower(h.Connection) == "winrm" {
		pw, err := winrmPassword(h)
		if err != nil {
			return nil, err
		}
		return ConnectWinRM(h, pw)
	}
	return Connect(h)
}

// isWinRM reports whether a session speaks WinRM (module branching).
func isWinRM(s Remote) bool {
	_, ok := s.(*WinRMSession)
	return ok
}

// waitForRebootWinRM polls a WinRM host until it answers again.
func waitForRebootWinRM(s *WinRMSession, timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.Run("$env:COMPUTERNAME"); err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("WinRM did not return within %ds on %s", timeoutSec, s.Host)
}
