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
	Src  string // local path on the control node
	Dest string // remote path on the target host
}

func (m CopyModule) Name() string { return "copy" }

func (m CopyModule) Check(s *Session) (bool, error) {
	localHash, err := localFileSHA256(m.Src)
	if err != nil {
		return false, fmt.Errorf("reading local file '%s': %w", m.Src, err)
	}
	out, _ := s.Run("sha256sum " + m.Dest + " 2>/dev/null | awk '{print $1}'")
	remoteHash := strings.TrimSpace(out)
	return localHash != remoteHash, nil
}

func (m CopyModule) Apply(s *Session) (string, error) {
	if err := s.Upload(m.Src, m.Dest); err != nil {
		return "", err
	}
	return fmt.Sprintf("copied → %s", m.Dest), nil
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
