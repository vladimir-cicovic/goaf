package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// RunReport holds a structured summary of one goaf run for JSON/HTML output.
type RunReport struct {
	Timestamp string       `json:"timestamp"`
	CheckMode bool         `json:"check_mode"`
	Mode      string       `json:"mode"` // "adhoc" or "playbook"
	Summary   ReportStats  `json:"summary"`
	Hosts     []ReportHost `json:"hosts"`
}

type ReportStats struct {
	Ok      int `json:"ok"`
	Changed int `json:"changed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type ReportHost struct {
	Host    string `json:"host"`
	Ok      int    `json:"ok"`
	Changed int    `json:"changed"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
}

func newReport(mode string, checkMode bool) *RunReport {
	return &RunReport{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Mode:      mode,
		CheckMode: checkMode,
	}
}

func (r *RunReport) addHost(label string, s *hostStats) {
	r.Hosts = append(r.Hosts, ReportHost{
		Host:    label,
		Ok:      s.ok,
		Changed: s.changed,
		Failed:  s.failed,
		Skipped: s.skipped,
	})
	r.Summary.Ok += s.ok
	r.Summary.Changed += s.changed
	r.Summary.Failed += s.failed
	r.Summary.Skipped += s.skipped
}

// writeReport writes a JSON or HTML report to path (format determined by .json extension).
func writeReport(r *RunReport, path string) error {
	if strings.HasSuffix(path, ".json") {
		return writeJSONReport(r, path)
	}
	return writeHTMLReport(r, path)
}

func writeJSONReport(r *RunReport, path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func writeHTMLReport(r *RunReport, path string) error {
	modeStr := r.Mode
	if r.CheckMode {
		modeStr += " (check mode)"
	}

	var rows strings.Builder
	for _, h := range r.Hosts {
		rows.WriteString(fmt.Sprintf(
			"<tr><td>%s</td><td class=\"ok\">%d</td><td class=\"changed\">%d</td><td class=\"failed\">%d</td><td>%d</td></tr>\n",
			h.Host, h.Ok, h.Changed, h.Failed, h.Skipped,
		))
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>GOAF Run Report</title>
<style>
body{font-family:monospace;margin:20px;background:#1e1e1e;color:#d4d4d4}
h1{color:#4fc3f7}h2{color:#81c784}
table{border-collapse:collapse;width:100%%;max-width:800px}
th,td{border:1px solid #555;padding:6px 12px;text-align:left}
th{background:#2d2d2d;color:#4fc3f7}
tr:nth-child(even){background:#252525}
.ok{color:#81c784}.changed{color:#ffb74d}.failed{color:#ef5350}
.meta{color:#9e9e9e}
</style>
</head>
<body>
<h1>GOAF Run Report</h1>
<div class="meta">
<p>Timestamp: %s</p>
<p>Mode: %s</p>
</div>
<h2>Summary</h2>
<table>
<tr><th>ok</th><th>changed</th><th>failed</th><th>skipped</th></tr>
<tr><td class="ok">%d</td><td class="changed">%d</td><td class="failed">%d</td><td>%d</td></tr>
</table>
<h2>Hosts</h2>
<table>
<tr><th>Host</th><th>ok</th><th>changed</th><th>failed</th><th>skipped</th></tr>
%s</table>
</body>
</html>
`,
		r.Timestamp, modeStr,
		r.Summary.Ok, r.Summary.Changed, r.Summary.Failed, r.Summary.Skipped,
		rows.String(),
	)

	return os.WriteFile(path, []byte(html), 0644)
}
