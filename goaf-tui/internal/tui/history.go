package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunSummary describes one recorded run for the history listing.
type RunSummary struct {
	ID      string `json:"id"`
	Time    string `json:"time"`
	Mode    string `json:"mode"`
	Ok      int    `json:"ok"`
	Changed int    `json:"changed"`
	Failed  int    `json:"failed"`
}

const maxHistoryEntries = 50

func historyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".goaf", "history")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// runHistoryFile creates the NDJSON record file for a run.
// Returns nil file (with empty id) when history is unavailable —
// recording must never break a run.
func runHistoryFile(mode string) (*os.File, string) {
	dir, err := historyDir()
	if err != nil {
		return nil, ""
	}
	id := time.Now().Format("20060102-150405") + "-" + mode
	f, err := os.Create(filepath.Join(dir, id+".ndjson"))
	if err != nil {
		return nil, ""
	}
	return f, id
}

// finalizeHistoryRun extracts the run_finished summary from the record,
// writes the sidecar summary file and rotates old entries.
func finalizeHistoryRun(id string) {
	if id == "" {
		return
	}
	dir, err := historyDir()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".ndjson"))
	if err != nil {
		return
	}
	summary := RunSummary{ID: id, Time: time.Now().Format(time.RFC3339)}
	if i := strings.LastIndex(id, "-"); i >= 0 && i+1 < len(id) {
		summary.Mode = id[i+1:]
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev["type"] != "run_finished" {
			continue
		}
		num := func(k string) int { v, _ := ev[k].(float64); return int(v) }
		summary.Ok = num("ok")
		summary.Changed = num("changed")
		summary.Failed = num("failed")
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, id+".summary.json"), data, 0o644)
	rotateHistory(dir)
}

func rotateHistory(dir string) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.summary.json"))
	if err != nil || len(entries) <= maxHistoryEntries {
		return
	}
	sort.Strings(entries) // chronological: IDs start with timestamp
	for _, old := range entries[:len(entries)-maxHistoryEntries] {
		_ = os.Remove(old)
		_ = os.Remove(strings.TrimSuffix(old, ".summary.json") + ".ndjson")
	}
}

// ListHistory returns recorded run summaries, newest first.
func ListHistory() ([]RunSummary, error) {
	dir, err := historyDir()
	if err != nil {
		return nil, err
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.summary.json"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(entries)))
	var out []RunSummary
	for _, e := range entries {
		data, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		var s RunSummary
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// loadHistoryLines replays a recorded run: parses its NDJSON and formats
// each event exactly like a live run (host status table untouched).
func loadHistoryLines(id string) ([]string, error) {
	dir, err := historyDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".ndjson"))
	if err != nil {
		return nil, err
	}
	out := []string{"REPLAY " + id + "  (Esc to exit)", ""}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "" {
			continue
		}
		out = append(out, formatEvent(eventMsg{eventType: typ, data: m})...)
	}
	return out, nil
}

// runMode guesses a short mode label from goaf args for the history id.
func runMode(args []string) string {
	for i, a := range args {
		if a == "run" && i+1 < len(args) {
			return "playbook"
		}
	}
	skipNext := false
	takesValue := map[string]bool{
		"-i": true, "-t": true, "-p": true, "-report": true,
		"--limit": true, "--serial": true, "--tags": true, "--skip-tags": true,
		"--vault-pass-file": true,
	}
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			base := a
			if i := strings.Index(a, "="); i >= 0 {
				base = a[:i]
			}
			skipNext = takesValue[base]
			continue
		}
		if a != "" {
			return a
		}
	}
	return "adhoc"
}
