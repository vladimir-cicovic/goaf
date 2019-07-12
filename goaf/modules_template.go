package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"text/template"
)

// TemplateModule renders a Go text/template with variables and deploys the result
// to the target host via SFTP.
// Idempotency: compares SHA256 of rendered content with the remote file.
type TemplateModule struct {
	Src  string            // local template path
	Dest string            // remote path on target host
	Vars map[string]string // variables available in the template as {{.key}}
}

func (m TemplateModule) Name() string { return "template" }

func (m TemplateModule) Check(s *Session) (bool, error) {
	rendered, err := m.render()
	if err != nil {
		return false, err
	}
	localHash := bytesSHA256(rendered)
	out, _ := s.Run("sha256sum " + m.Dest + " 2>/dev/null | awk '{print $1}'")
	remoteHash := strings.TrimSpace(out)
	return localHash != remoteHash, nil
}

func (m TemplateModule) Apply(s *Session) (string, error) {
	rendered, err := m.render()
	if err != nil {
		return "", err
	}
	if err := s.UploadContent(rendered, m.Dest); err != nil {
		return "", err
	}
	return fmt.Sprintf("template deployed → %s", m.Dest), nil
}

func (m TemplateModule) render() ([]byte, error) {
	src, err := os.ReadFile(m.Src)
	if err != nil {
		return nil, fmt.Errorf("reading template '%s': %w", m.Src, err)
	}
	tmpl, err := template.New("").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}
	// Convert to map[string]interface{} so {{.key}} syntax works
	data := make(map[string]interface{}, len(m.Vars))
	for k, v := range m.Vars {
		data[k] = v
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing template: %w", err)
	}
	return buf.Bytes(), nil
}

func bytesSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
