package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// activeEmitter is the package-level emitter used throughout a run.
// Set to JSONEmitter when -json flag is present, TextEmitter otherwise.
var activeEmitter Emitter = &TextEmitter{}

// Emitter sends run lifecycle events to the appropriate output.
type Emitter interface {
	RunStarted(mode string, hostCount int, parallel int, checkMode bool)
	PlayStarted(name string, isFirst bool)
	GatheringFacts()
	FactsGathered(host string)
	TaskHeader(label string)
	TaskSkipped(host, reason string)
	TaskResult(r Result)
	HandlersRunning()
	HandlerHeader(name string)
	RecapStarted(checkMode bool)
	HostRecap(host string, ok, changed, failed, skipped int)
	RunFinished(ok, changed, failed int)
	Diagnostic(msg string)
}

// ### TextEmitter ###
// Preserves the existing human-readable output exactly as before.

type TextEmitter struct{}

func (e *TextEmitter) RunStarted(_ string, _ int, _ int, _ bool) {}

func (e *TextEmitter) PlayStarted(name string, isFirst bool) {
	if !isFirst {
		fmt.Println()
	}
	printHeader("PLAY [" + name + "]")
}

func (e *TextEmitter) GatheringFacts() {
	fmt.Println()
	printHeader("TASK [Gathering Facts]")
}

func (e *TextEmitter) FactsGathered(host string) {
	fmt.Printf("ok: [%s]\n", host)
}

func (e *TextEmitter) TaskHeader(label string) {
	fmt.Println()
	printHeader("TASK [" + label + "]")
}

func (e *TextEmitter) TaskSkipped(host, reason string) {
	fmt.Printf("skipping: [%s] (%s)\n", host, reason)
}

func (e *TextEmitter) TaskResult(r Result) {
	switch {
	case r.Err != nil:
		fmt.Printf("FAILED: [%s]\n  MSG: %v\n", r.Host, r.Err)
	case r.Changed && r.DryRun:
		fmt.Printf("would change: [%s]\n", r.Host)
	case r.Changed:
		if r.Output != "" {
			fmt.Printf("changed: [%s] => %s\n", r.Host, r.Output)
		} else {
			fmt.Printf("changed: [%s]\n", r.Host)
		}
	default:
		fmt.Printf("ok: [%s]\n", r.Host)
	}
}

func (e *TextEmitter) HandlersRunning() {
	fmt.Println()
	printHeader("RUNNING HANDLERS")
}

func (e *TextEmitter) HandlerHeader(name string) {
	fmt.Println()
	printHeader("HANDLER [" + name + "]")
}

func (e *TextEmitter) RecapStarted(checkMode bool) {
	fmt.Println()
	recap := "PLAY RECAP"
	if checkMode {
		recap += " (CHECK MODE — no changes applied)"
	}
	printHeader(recap)
}

func (e *TextEmitter) HostRecap(host string, ok, changed, failed, skipped int) {
	fmt.Printf("%-30s : ok=%-4d changed=%-4d failed=%-4d skipped=%d\n",
		host, ok, changed, failed, skipped)
}

func (e *TextEmitter) RunFinished(_ int, _ int, _ int) {}

func (e *TextEmitter) Diagnostic(msg string) {
	fmt.Println(msg)
}

// ### JSONEmitter ###
// Emits one JSON object per line (NDJSON) on stdout.
// Diagnostic messages go to stderr and never mix with the event stream.

type JSONEmitter struct {
	enc *json.Encoder
}

func newJSONEmitter() *JSONEmitter {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return &JSONEmitter{enc: enc}
}

func (e *JSONEmitter) emit(v any) {
	_ = e.enc.Encode(v) // Encode appends \n automatically
}

func (e *JSONEmitter) RunStarted(mode string, hostCount int, parallel int, checkMode bool) {
	e.emit(map[string]any{
		"type":       "run_started",
		"mode":       mode,
		"host_count": hostCount,
		"parallel":   parallel,
		"check_mode": checkMode,
	})
}

func (e *JSONEmitter) PlayStarted(name string, _ bool) {
	e.emit(map[string]any{"type": "play_started", "play": name})
}

func (e *JSONEmitter) GatheringFacts() {
	e.emit(map[string]any{"type": "gathering_facts"})
}

func (e *JSONEmitter) FactsGathered(host string) {
	e.emit(map[string]any{"type": "facts_gathered", "host": host})
}

func (e *JSONEmitter) TaskHeader(label string) {
	e.emit(map[string]any{"type": "task_started", "task": label})
}

func (e *JSONEmitter) TaskSkipped(host, reason string) {
	e.emit(map[string]any{
		"type":   "task_result",
		"host":   host,
		"status": "skipped",
		"reason": reason,
	})
}

func (e *JSONEmitter) TaskResult(r Result) {
	ev := map[string]any{"type": "task_result", "host": r.Host}
	switch {
	case r.Err != nil:
		ev["status"] = "failed"
		ev["error"] = r.Err.Error()
	case r.Changed && r.DryRun:
		ev["status"] = "would_change"
		ev["output"] = r.Output
	case r.Changed:
		ev["status"] = "changed"
		ev["output"] = r.Output
	default:
		ev["status"] = "ok"
		ev["output"] = r.Output
	}
	e.emit(ev)
}

func (e *JSONEmitter) HandlersRunning() {
	e.emit(map[string]any{"type": "handlers_running"})
}

func (e *JSONEmitter) HandlerHeader(name string) {
	e.emit(map[string]any{"type": "handler_started", "handler": name})
}

func (e *JSONEmitter) RecapStarted(checkMode bool) {
	e.emit(map[string]any{"type": "recap_started", "check_mode": checkMode})
}

func (e *JSONEmitter) HostRecap(host string, ok, changed, failed, skipped int) {
	e.emit(map[string]any{
		"type":    "host_recap",
		"host":    host,
		"ok":      ok,
		"changed": changed,
		"failed":  failed,
		"skipped": skipped,
	})
}

func (e *JSONEmitter) RunFinished(ok, changed, failed int) {
	e.emit(map[string]any{
		"type":    "run_finished",
		"ok":      ok,
		"changed": changed,
		"failed":  failed,
	})
}

func (e *JSONEmitter) Diagnostic(msg string) {
	e.emit(map[string]any{"type": "diagnostic", "msg": msg})
}
