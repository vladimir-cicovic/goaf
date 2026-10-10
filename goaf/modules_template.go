package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"text/template"
)

// TemplateModule renders a Go text/template with variables and deploys the result
// to the target host via SFTP.
// Idempotency: compares SHA256 of rendered content with the remote file.
type TemplateModule struct {
	Src    string            // local template path
	Dest   string            // remote path on target host
	Vars   map[string]string // variables available in the template as {{.key}}
	Backup bool              // save existing remote file to dest.goafbak-<timestamp> before replacing
}

func (m TemplateModule) Name() string { return "template" }

func (m TemplateModule) Check(s Remote) (bool, error) {
	rendered, err := m.render()
	if err != nil {
		return false, err
	}
	// Control-side compare: no remote hashing tools needed (works over WinRM).
	old, rerr := s.ReadRemote(m.Dest)
	if rerr != nil {
		return true, nil // missing remote file → must deploy
	}
	return sha256.Sum256(rendered) != sha256.Sum256(old), nil
}

func (m TemplateModule) Apply(s Remote) (string, error) {
	rendered, err := m.render()
	if err != nil {
		return "", err
	}
	msg := ""
	if m.Backup {
		if bk, berr := backupRemote(s, m.Dest); berr != nil {
			return "", berr
		} else if bk != "" {
			msg = "backup: " + bk + "\n"
		}
	}
	if err := s.UploadContent(rendered, m.Dest); err != nil {
		return "", err
	}
	return msg + fmt.Sprintf("template deployed → %s", m.Dest), nil
}

// Diff shows the unified diff of current remote content vs rendered template.
func (m TemplateModule) Diff(s Remote) (string, error) {
	rendered, err := m.render()
	if err != nil {
		return "", err
	}
	old, _ := s.ReadRemote(m.Dest) // missing file → treated as empty
	return unifiedDiff(m.Dest, m.Dest, string(old), string(rendered), 3), nil
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
