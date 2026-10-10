package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stateEntry records one applied change for drift detection.
type stateEntry struct {
	Time        string            `json:"time"`
	Host        string            `json:"host"` // label, addr or addr:port
	Addr        string            `json:"addr"`
	Port        int               `json:"port"`
	User        string            `json:"user"`
	Key         string            `json:"key"`
	Play        string            `json:"play"`
	Module      string            `json:"module"`
	Params      map[string]string `json:"params"` // expanded at apply time
	Output      string            `json:"output"`
	Fingerprint string            `json:"fingerprint"` // host+module+params hash
}

const maxStateEntries = 2000

// statePath returns ~/.goaf/state.json ("" when home is unknown).
func statePath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".goaf", "state.json")
}

// fingerprintOf identifies one managed piece: host + module + params.
func fingerprintOf(host, module string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(host)
	b.WriteString("\x00")
	b.WriteString(module)
	for _, k := range keys {
		b.WriteString("\x00")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum)[:16]
}

// recordState appends an applied change (deduplicated by fingerprint,
// newest wins; best effort — state must never break a run).
func recordState(h Host, play, module string, params map[string]string, output string) {
	path := statePath()
	if path == "" {
		return
	}
	label := hostLabel(h)
	entry := stateEntry{
		Time:        time.Now().UTC().Format(time.RFC3339),
		Host:        label,
		Addr:        h.Addr,
		Port:        h.Port,
		User:        h.User,
		Key:         h.Key,
		Play:        play,
		Module:      module,
		Params:      params,
		Output:      output,
		Fingerprint: fingerprintOf(label, module, params),
	}
	entries := loadState()
	kept := make([]stateEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.Fingerprint != entry.Fingerprint {
			kept = append(kept, e)
		}
	}
	kept = append(kept, entry)
	if len(kept) > maxStateEntries {
		kept = kept[len(kept)-maxStateEntries:]
	}
	saveStateEntries(kept)
}

// saveStateEntries persists entries (best effort).
func saveStateEntries(entries []stateEntry) {
	path := statePath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// forgetState removes entries matching a host label or fingerprint prefix.
// Returns removed count.
func forgetState(arg string) int {
	entries := loadState()
	kept := entries[:0]
	removed := 0
	for _, e := range entries {
		if e.Host == arg || strings.HasPrefix(e.Fingerprint, arg) {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	if removed > 0 {
		saveStateEntries(kept)
	}
	return removed
}

// loadState reads all recorded entries (empty on any problem).
func loadState() []stateEntry {
	path := statePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []stateEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return entries
}
