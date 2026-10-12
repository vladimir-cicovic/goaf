package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
	Connection  string            `json:"connection"`
	Play        string            `json:"play"`
	Module      string            `json:"module"`
	Params      map[string]string `json:"params"` // expanded at apply time
	Output      string            `json:"output"`
	Fingerprint string            `json:"fingerprint"` // host+module+params hash
}

const maxStateEntries = 2000

// statePath returns ~/.goaf/state.json ("" when home is unknown).
// Legacy global location; prefer statePathFor.
func statePath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".goaf", "state.json")
}

// stateScopeKey hashes inventory path + workspace into a short id.
func stateScopeKey(invPath, workspace string) string {
	sum := sha256.Sum256([]byte(invPath + "\x00" + workspace))
	return fmt.Sprintf("%x", sum)[:12]
}

// statePathFor scopes state by inventory + workspace:
// ~/.goaf/state-<hash>.json. A legacy global state.json migrates once:
// into the first scope used when no scoped file exists yet.
func statePathFor(inv *Inventory) string {
	home := homeDir()
	if home == "" {
		return ""
	}
	invPath, workspace := "", workspaceName
	if inv != nil {
		invPath = inv.invPath
	}
	if invPath == "" {
		return filepath.Join(home, ".goaf", "state.json")
	}
	scoped := filepath.Join(home, ".goaf", "state-"+stateScopeKey(invPath, workspace)+".json")
	if _, err := os.Stat(scoped); err != nil {
		if existing, _ := filepath.Glob(filepath.Join(home, ".goaf", "state-*.json")); len(existing) == 0 {
			migrateLegacyState(home, scoped, inv)
		}
	}
	return scoped
}

// migrateLegacyState copies legacy global entries matching the current
// inventory into a fresh scope (one-time, first scoped use).
func migrateLegacyState(home, scoped string, inv *Inventory) {
	data, err := os.ReadFile(filepath.Join(home, ".goaf", "state.json"))
	if err != nil {
		return
	}
	if inv == nil {
		_ = os.WriteFile(scoped, data, 0o644)
		return
	}
	known := map[string]bool{}
	if inv.Groups != nil {
		for name := range inv.Groups {
			hosts, err := inv.Resolve(name)
			if err != nil {
				continue
			}
			for _, h := range hosts {
				known[hostLabel(h)] = true
			}
		}
	}
	var entries []stateEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	kept := entries[:0]
	for _, e := range entries {
		if known[e.Host] {
			kept = append(kept, e)
		}
	}
	if out, err := json.MarshalIndent(kept, "", "  "); err == nil {
		_ = os.WriteFile(scoped, out, 0o644)
	}
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
func recordState(inv *Inventory, h Host, play, module string, params map[string]string, output string) {
	path := statePathFor(inv)
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
		Connection:  h.Connection,
		Play:        play,
		Module:      module,
		Params:      params,
		Output:      output,
		Fingerprint: fingerprintOf(label, module, params),
	}
	recordStateLocked(statePathFor(inv), entry)
}

// recordStateLocked is recordState with read-modify-write under one lock
// (used when correctness across parallel runs matters).
func recordStateLocked(path string, entry stateEntry) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = withLockedFile(path, os.O_RDWR|os.O_CREATE, 0o644, func(f *os.File) error {
		data, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		var entries []stateEntry
		if len(data) > 0 {
			if err := json.Unmarshal(data, &entries); err != nil {
				entries = nil // corrupt file: start over
			}
		}
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
		out, err := json.MarshalIndent(kept, "", "  ")
		if err != nil {
			return err
		}
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
		_, err = f.Write(out)
		return err
	})
}

// saveStateEntries persists entries (best effort).
func saveStateEntries(entries []stateEntry) {
	saveStateEntriesFor(nil, entries)
}

// saveStateEntriesFor persists entries to the scoped file (locked:
// parallel runs share one state file).
func saveStateEntriesFor(inv *Inventory, entries []stateEntry) {
	path := statePathFor(inv)
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	_ = withLockedFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// forgetState removes entries matching a host label or fingerprint prefix.
// Returns removed count.
func forgetState(inv *Inventory, arg string) int {
	entries := loadStateFor(inv)
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
		saveStateEntriesFor(inv, kept)
	}
	return removed
}

// loadState reads all recorded entries (empty on any problem).
func loadState() []stateEntry {
	return loadStateFor(nil)
}

// loadStateFor reads entries from the scoped file.
func loadStateFor(inv *Inventory) []stateEntry {
	path := statePathFor(inv)
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
