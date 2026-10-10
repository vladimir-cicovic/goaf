package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// auditRecord is one row in the audit log: who ran what, when,
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

const auditSchema = `CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  time TEXT NOT NULL,
  user TEXT NOT NULL,
  mode TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  playbook TEXT NOT NULL DEFAULT '',
  hosts INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  changed INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0,
  check_mode INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0
);`

// auditDBPath returns ~/.goaf/audit.db ("" when home is unknown).
func auditDBPath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".goaf", "audit.db")
}

// openAuditDB opens (and migrates) the audit database.
func openAuditDB() (*sql.DB, error) {
	path := auditDBPath()
	if path == "" {
		return nil, os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(auditSchema); err != nil {
		db.Close()
		return nil, err
	}
	migrateAuditLog(db)
	return db, nil
}

// migrateAuditLog imports legacy ~/.goaf/audit.log JSONL once.
func migrateAuditLog(db *sql.DB) {
	legacy := filepath.Join(filepath.Dir(auditDBPath()), "audit.log")
	data, err := os.ReadFile(legacy)
	if err != nil {
		return
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM runs").Scan(&count)
	if count > 0 {
		return // already migrated (or has newer rows)
	}
	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO runs
	  (id, time, user, mode, target, playbook, hosts, ok, changed, failed, check_mode, duration_ms)
	  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return
	}
	defer stmt.Close()
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec auditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.ID == "" {
			rec.ID = rec.Time
		}
		check := 0
		if rec.CheckMode {
			check = 1
		}
		if _, err := stmt.Exec(rec.ID, rec.Time, rec.User, rec.Mode, rec.Target,
			rec.Playbook, rec.Hosts, rec.Ok, rec.Changed, rec.Failed, check, rec.Ms); err != nil {
			continue
		}
		n++
	}
	_ = tx.Commit()
	if n > 0 {
		_ = os.Rename(legacy, legacy+".migrated")
	}
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

// appendAudit records one run (best effort — audit never breaks a run).
func appendAudit(rec auditRecord) {
	db, err := openAuditDB()
	if err != nil {
		return
	}
	defer db.Close()
	rec.Time = time.Now().UTC().Format(time.RFC3339)
	rec.ID = time.Now().Format("20060102-150405")
	var seq int
	_ = db.QueryRow("SELECT COUNT(*) FROM runs WHERE id LIKE ?", rec.ID+"%").Scan(&seq)
	if seq > 0 {
		rec.ID = strings.Join([]string{rec.ID, string(rune('a' + seq - 1))}, "-")
	}
	if rec.User == "" {
		rec.User = auditUser()
	}
	check := 0
	if rec.CheckMode {
		check = 1
	}
	_, _ = db.Exec(`INSERT OR IGNORE INTO runs
	  (id, time, user, mode, target, playbook, hosts, ok, changed, failed, check_mode, duration_ms)
	  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Time, rec.User, rec.Mode, rec.Target, rec.Playbook,
		rec.Hosts, rec.Ok, rec.Changed, rec.Failed, check, rec.Ms)
}

// listAudit returns the last n records (oldest first).
func listAudit(n int) []auditRecord {
	db, err := openAuditDB()
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, time, user, mode, target, playbook, hosts, ok, changed, failed, check_mode, duration_ms
	  FROM runs ORDER BY time DESC, id DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var all []auditRecord
	for rows.Next() {
		var rec auditRecord
		var check int
		if err := rows.Scan(&rec.ID, &rec.Time, &rec.User, &rec.Mode, &rec.Target,
			&rec.Playbook, &rec.Hosts, &rec.Ok, &rec.Changed, &rec.Failed, &check, &rec.Ms); err != nil {
			continue
		}
		rec.CheckMode = check != 0
		all = append(all, rec)
	}
	// newest-first from SQL → reverse to oldest-first, then keep last n.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}
