package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

// CopyModule copies a file from the control node to the target host via SFTP.
// Idempotency: compares SHA256 of local and remote file.
type CopyModule struct {
	Src    string // local path on the control node
	Dest   string // remote path on the target host
	Backup bool   // save existing remote file to dest.goafbak-<timestamp> before replacing
}

// backupRemote copies an existing remote file to a timestamped backup.
// Returns the backup path, or "" when there was nothing to back up.
func backupRemote(s Remote, dest string) (string, error) {
	if isWinRM(s) {
		return backupRemoteWin(dest, s)
	}
	out, err := s.Run("test -e " + shQuote(dest) + " && date +%Y%m%d%H%M%S")
	if err != nil {
		return "", nil // missing file — nothing to back up (not fatal)
	}
	ts := strings.TrimSpace(out)
	if ts == "" {
		return "", nil
	}
	bk := dest + ".goafbak-" + ts
	if _, err := s.Run("cp -p " + shQuote(dest) + " " + shQuote(bk)); err != nil {
		return "", err
	}
	return bk, nil
}

// backupRemoteWin is backupRemote for Windows targets (PowerShell).
func backupRemoteWin(dest string, s Remote) (string, error) {
	exists, _ := s.Run("if (Test-Path " + psQuote(dest) + ") { 'yes' } else { 'no' }")
	if strings.TrimSpace(exists) != "yes" {
		return "", nil
	}
	ts, err := s.Run("Get-Date -Format 'yyyyMMddHHmmss'")
	if err != nil {
		return "", nil
	}
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return "", nil
	}
	bk := dest + ".goafbak-" + ts
	if _, err := s.Run("Copy-Item -Path " + psQuote(dest) + " -Destination " + psQuote(bk) + " -Force"); err != nil {
		return "", err
	}
	return bk, nil
}

func (m CopyModule) Name() string { return "copy" }

func (m CopyModule) Check(s Remote) (bool, error) {
	want, err := os.ReadFile(m.Src)
	if err != nil {
		return false, fmt.Errorf("reading local file '%s': %w", m.Src, err)
	}
	// Control-side compare: no remote hashing tools needed (works over WinRM).
	old, rerr := s.ReadRemote(m.Dest)
	if rerr != nil {
		return true, nil // missing remote file → must copy
	}
	return sha256.Sum256(want) != sha256.Sum256(old), nil
}

func (m CopyModule) Apply(s Remote) (string, error) {
	msg := ""
	if m.Backup {
		if bk, err := backupRemote(s, m.Dest); err != nil {
			return "", err
		} else if bk != "" {
			msg = "backup: " + bk + "\n"
		}
	}
	if err := s.Upload(m.Src, m.Dest); err != nil {
		return "", err
	}
	return msg + fmt.Sprintf("copied → %s", m.Dest), nil
}

// Diff shows the unified diff of current remote content vs the local file.
func (m CopyModule) Diff(s Remote) (string, error) {
	want, err := os.ReadFile(m.Src)
	if err != nil {
		return "", err
	}
	old, _ := s.ReadRemote(m.Dest) // missing file → treated as empty
	return unifiedDiff(m.Dest, m.Dest, string(old), string(want), 3), nil
}
