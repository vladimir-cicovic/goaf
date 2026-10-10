package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// notifyURL / notifyOn come from --notify-webhook / --notify-on.
var notifyURL string
var notifyOn string

// notifyPayload is POSTed to --notify-webhook at run end.
// Slack renders the text field; anything else can parse the rest.
type notifyPayload struct {
	Text    string `json:"text"`
	Mode    string `json:"mode"`
	Target  string `json:"target,omitempty"`
	Hosts   int    `json:"hosts"`
	Ok      int    `json:"ok"`
	Changed int    `json:"changed"`
	Failed  int    `json:"failed"`
	When    string `json:"when"`
}

// sendNotify POSTs the recap as JSON (best effort, 10s timeout).
// onMode filters: "always" (default) or "failure" (only when failed > 0).
func sendNotify(url, onMode, mode, target string, hosts, ok, changed, failed int) {
	if url == "" {
		return
	}
	if onMode != "" && onMode != "always" && onMode != "failure" {
		fmt.Fprintf(os.Stderr, "notify: unknown --notify-on %q (want always|failure)\n", onMode)
		return
	}
	if onMode == "failure" && failed == 0 {
		return
	}
	status := "passed"
	if failed > 0 {
		status = "FAILED"
	}
	payload := notifyPayload{
		Text:    fmt.Sprintf("goaf %s %s: %s (hosts=%d ok=%d changed=%d failed=%d)", mode, target, status, hosts, ok, changed, failed),
		Mode:    mode,
		Target:  target,
		Hosts:   hosts,
		Ok:      ok,
		Changed: changed,
		Failed:  failed,
		When:    time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notify: %v\n", err)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "notify: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "notify: webhook returned %s\n", resp.Status)
	}
}
