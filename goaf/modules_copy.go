package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
func backupRemote(s *Session, dest string) (string, error) {
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

func (m CopyModule) Name() string { return "copy" }

func (m CopyModule) Check(s *Session) (bool, error) {
	localHash, err := localFileSHA256(m.Src)
	if err != nil {
		return false, fmt.Errorf("reading local file '%s': %w", m.Src, err)
	}
	out, _ := s.Run("sha256sum " + shQuote(m.Dest) + " 2>/dev/null | awk '{print $1}'")
	remoteHash := strings.TrimSpace(out)
	return localHash != remoteHash, nil
}

func (m CopyModule) Apply(s *Session) (string, error) {
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
func (m CopyModule) Diff(s *Session) (string, error) {
	want, err := os.ReadFile(m.Src)
	if err != nil {
		return "", err
	}
	old, _ := s.ReadRemote(m.Dest) // missing file → treated as empty
	return unifiedDiff(m.Dest, m.Dest, string(old), string(want), 3), nil
}

func localFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
