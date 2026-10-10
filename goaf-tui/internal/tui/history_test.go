package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // windows fallback used by os.UserHomeDir
	return dir
}

func TestHistoryRecordFinalizeList(t *testing.T) {
	testHome(t)

	f, id := runHistoryFile("adhoc")
	if f == nil || id == "" {
		t.Fatal("no history file")
	}
	_, _ = f.WriteString("{\"type\":\"run_started\"}\n")
	_, _ = f.WriteString("{\"type\":\"run_finished\",\"ok\":3,\"changed\":1,\"failed\":0}\n")
	_ = f.Close()

	finalizeHistoryRun(id)

	runs, err := ListHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].Ok != 3 || runs[0].Changed != 1 || runs[0].Failed != 0 {
		t.Errorf("bad summary: %+v", runs[0])
	}
	if runs[0].Mode != "adhoc" {
		t.Errorf("bad mode: %q", runs[0].Mode)
	}
}

func TestHistoryRotate(t *testing.T) {
	dir := testHome(t)
	dir = filepath.Join(dir, ".goaf", "history")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxHistoryEntries+5; i++ {
		id := "20000101-000000-run" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('0'+i%10))
		_ = os.WriteFile(filepath.Join(dir, id+".ndjson"), []byte("{}\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, id+".summary.json"), []byte("{}"), 0o644)
	}
	// one more finalize triggers rotation
	f, id := runHistoryFile("adhoc")
	if f == nil {
		t.Fatal("no file")
	}
	_, _ = f.WriteString("{}\n")
	_ = f.Close()
	finalizeHistoryRun(id)

	runs, err := ListHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) > maxHistoryEntries {
		t.Errorf("rotation failed: %d entries", len(runs))
	}
}

func TestLoadHistoryLines(t *testing.T) {
	testHome(t)
	dir, err := historyDir()
	if err != nil {
		t.Fatal(err)
	}
	id := "20240101-120000-adhoc"
	ndjson := "{\"type\":\"task_started\",\"task\":\"uptime\"}\n" +
		"{\"type\":\"task_result\",\"host\":\"h1\",\"status\":\"changed\",\"output\":\"hi\"}\n" +
		"{\"type\":\"play_output\",\"name\":\"ver\",\"value\":\"1.0\"}\n" +
		"not-json\n" +
		"{\"type\":\"run_finished\",\"ok\":1,\"changed\":1,\"failed\":0}\n"
	if err := os.WriteFile(filepath.Join(dir, id+".ndjson"), []byte(ndjson), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := loadHistoryLines(id)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"REPLAY " + id, "uptime", "h1", "hi", "output: ver = 1.0", "Done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("replay missing %q:\n%s", want, joined)
		}
	}
	if _, err := loadHistoryLines("nope-missing"); err == nil {
		t.Error("expected error for missing run")
	}
}

func TestRunMode(t *testing.T) {
	if got := runMode([]string{"-i", "inv.yml", "run", "site.yml"}); got != "playbook" {
		t.Errorf("got %q", got)
	}
	if got := runMode([]string{"-t", "web", "command", "uptime"}); got != "command" {
		t.Errorf("got %q", got)
	}
}
