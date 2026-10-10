package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// auditRecord is one JSON line in the audit log: who ran what, when,
// on how many hosts, with what outcome.
type auditRecord struct {
	ID        string `json:"id"` // 20060102-150405
	Time      string `json:"time"`
	User      string `json:"user"`
	Mode      string `json:"mode"` // adhoc, playbook, apply
	Target    string `json:"target,omitempty"`
	Playbook  string `json:"playbook,omitempty"`
	Hosts     int    `json:"hosts"`
	Ok        int    `json:"ok"`
	Changed   int    `json:"changed"`
	Failed    int    `json:"failed"`
	CheckMode bool   `json:"check_mode"`
	Ms        int64  `json:"duration_ms"`
}

// auditPath returns ~/.goaf/audit.log ("" when home is unknown).
func auditPath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".goaf", "audit.log")
}

// auditUser names the invoking OS user (best effort).
func auditUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "unknown"
}

// appendAudit appends one record (best effort — audit never breaks a run).
func appendAudit(rec auditRecord) {
	path := auditPath()
	if path == "" {
		return
	}
	rec.Time = time.Now().UTC().Format(time.RFC3339)
	rec.ID = time.Now().Format("20060102-150405")
	if rec.User == "" {
		rec.User = auditUser()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// listAudit returns the last n records (oldest first).
func listAudit(n int) []auditRecord {
	path := auditPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var all []auditRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		all = append(all, rec)
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}
