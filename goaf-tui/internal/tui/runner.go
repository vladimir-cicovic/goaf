package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"
)

// ### Message types ###

type eventMsg struct {
	eventType string
	data      map[string]any
}

type runDoneMsg struct{}

// ### Subprocess ###

// waitForEvent returns a tea.Cmd that blocks until the next event arrives on ch.
func waitForEvent(ch <-chan eventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return runDoneMsg{}
		}
		return msg
	}
}

// startRun launches the goaf binary with args, pipes its NDJSON stdout and
// stderr into a channel, and returns the channel. The channel is closed
// when the process exits.
func startRun(goafBin string, args []string) (*exec.Cmd, <-chan eventMsg, error) {
	cmd := exec.Command(goafBin, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	ch := make(chan eventMsg, 128)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		pipeNDJSON(stdout, ch)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		pipeStderr(stderr, ch)
	}()

	go func() {
		wg.Wait()
		_ = cmd.Wait()
		close(ch)
	}()

	return cmd, ch, nil
}

func pipeNDJSON(r io.Reader, ch chan<- eventMsg) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(line), &data); err != nil {
			continue
		}
		evType, _ := data["type"].(string)
		ch <- eventMsg{eventType: evType, data: data}
	}
}

func pipeStderr(r io.Reader, ch chan<- eventMsg) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			ch <- eventMsg{eventType: "stderr", data: map[string]any{"msg": line}}
		}
	}
}

// findGoaf searches for the goaf binary in PATH and common dev paths.
func findGoaf() (string, error) {
	if p, err := exec.LookPath("goaf"); err == nil {
		return p, nil
	}
	for _, p := range []string{"../Kod/goaf", "./goaf"} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("goaf not found in PATH or ../Kod/goaf — run: cd Kod && go build -o goaf .")
}

// ### Event formatting ###

// formatEvent converts one NDJSON event into display lines for the monitor panel.
func formatEvent(ev eventMsg) []string {
	str := func(k string) string { v, _ := ev.data[k].(string); return v }
	num := func(k string) int { v, _ := ev.data[k].(float64); return int(v) }
	b := func(k string) bool { v, _ := ev.data[k].(bool); return v }

	switch ev.eventType {
	case "run_started":
		s := "▶ Run: " + str("mode")
		if b("check_mode") {
			s += " [CHECK MODE]"
		}
		if h := num("host_count"); h > 0 {
			s += fmt.Sprintf("  %d hosts  parallel=%d", h, num("parallel"))
		}
		return []string{s}

	case "play_started":
		return []string{"", "PLAY [" + str("play") + "]"}

	case "gathering_facts":
		return []string{"  TASK [Gathering Facts]"}

	case "facts_gathered":
		return nil // too verbose

	case "task_started":
		return []string{"  TASK [" + str("task") + "]"}

	case "task_result":
		host, status := str("host"), str("status")
		out := str("output")
		reason := str("reason")
		errStr := str("error")

		// Split multi-line output: first line goes on the status line,
		// remaining lines are emitted indented below it.
		var extra []string
		if nl := strings.IndexByte(out, '\n'); nl >= 0 {
			lines := strings.Split(out, "\n")
			out = lines[0]
			for _, l := range lines[1:] {
				extra = append(extra, "        "+l)
			}
		}

		line := fmt.Sprintf("    [%s] %s", host, status)
		if out != "" {
			line += " → " + out
		}
		if reason != "" {
			line += " (" + reason + ")"
		}
		if errStr != "" {
			line += " ✗ " + errStr
		}
		return append([]string{line}, extra...)

	case "handlers_running":
		return []string{"  RUNNING HANDLERS"}

	case "handler_started":
		return []string{"  HANDLER [" + str("handler") + "]"}

	case "recap_started":
		hdr := "── PLAY RECAP"
		if b("check_mode") {
			hdr += " (CHECK MODE)"
		}
		return []string{"", hdr + " ─────────────────────────────"}

	case "host_recap":
		return []string{fmt.Sprintf("  %-24s ok=%-3d chg=%-3d fail=%-3d skip=%d",
			str("host"), num("ok"), num("changed"), num("failed"), num("skipped"))}

	case "run_finished":
		return []string{
			"",
			fmt.Sprintf("✓ Done   ok=%d  changed=%d  failed=%d",
				num("ok"), num("changed"), num("failed")),
		}

	case "diagnostic":
		return []string{"  ℹ " + str("msg")}

	case "stderr":
		return []string{"  ! " + str("msg")}
	}
	return nil
}

// colorLine applies lipgloss styles based on the content of a monitor line.
func colorLine(line string) string {
	switch {
	case strings.HasPrefix(line, "✓"):
		return styleOn.Render(line)
	case strings.HasPrefix(line, "PLAY ["):
		return lipgloss.NewStyle().Bold(true).Foreground(colorFocused).Render(line)
	case strings.HasPrefix(line, "── PLAY RECAP"), strings.HasPrefix(line, "── RECAP"):
		return lipgloss.NewStyle().Bold(true).Render(line)
	case strings.HasPrefix(line, "  TASK ["), strings.HasPrefix(line, "  HANDLER ["):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(line)
	case strings.Contains(line, "] failed"), strings.Contains(line, "✗"),
		strings.HasPrefix(line, "  !"):
		return styleErr.Render(line)
	case strings.Contains(line, "] changed"), strings.Contains(line, "] would_change"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(line)
	case strings.HasPrefix(line, "  ℹ"), strings.HasPrefix(line, "$"):
		return styleDim.Render(line)
	default:
		return line
	}
}
