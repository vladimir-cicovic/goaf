package main

import (
	"fmt"
	"strings"
)

type hostStats struct {
	ok      int
	changed int
	failed  int
	skipped int // plays skipped — host already processed in a previous play
}

// RunPlaybook executes all plays in a playbook and prints Ansible-style output.
// Each host is processed only in the first play it appears in; subsequent plays
// that target the same host skip it (cross-play deduplication).
// In checkMode, Apply is never called — tasks report "would change" instead.
// Returns the number of failed tasks and a structured RunReport.
func RunPlaybook(plays []Play, inv *Inventory, parallelism int, checkMode bool) (int, *RunReport) {
	report := newReport("playbook", checkMode)
	stats := make(map[string]*hostStats)
	var hostOrder []string
	registered := make(map[string]bool)
	processed := make(map[string]bool)

	totalFailed := 0

	for i, play := range plays {
		activeEmitter.PlayStarted(play.Name, i == 0)

		hosts, err := inv.Resolve(play.Hosts)
		if err != nil {
			activeEmitter.Diagnostic(fmt.Sprintf("ERROR resolving hosts %q: %v", play.Hosts, err))
			totalFailed++
			continue
		}

		// Register all hosts in stats (for PLAY RECAP ordering).
		for _, h := range hosts {
			label := hostLabel(h)
			if !registered[label] {
				registered[label] = true
				hostOrder = append(hostOrder, label)
				stats[label] = &hostStats{}
			}
		}

		// Split into fresh (not yet processed) and skipped.
		var fresh []Host
		for _, h := range hosts {
			if processed[hostLabel(h)] {
				label := hostLabel(h)
				activeEmitter.TaskSkipped(label, "already processed in a previous play")
				stats[label].skipped++
			} else {
				fresh = append(fresh, h)
			}
		}

		// Mark fresh hosts as processed before running tasks.
		for _, h := range fresh {
			processed[hostLabel(h)] = true
		}

		if len(fresh) == 0 {
			activeEmitter.Diagnostic("(no fresh hosts — play has nothing to do)")
			continue
		}

		// Gather host facts before tasks.
		var allFacts map[string]Facts
		if play.GatherFacts {
			activeEmitter.GatheringFacts()
			allFacts = GatherFactsAll(fresh, parallelism)
			for _, h := range fresh {
				activeEmitter.FactsGathered(hostLabel(h))
			}
		} else {
			allFacts = make(map[string]Facts)
		}

		notified := make(map[string]bool) // handler names triggered this play

		for _, task := range play.Tasks {
			// ### loop / single ###
			items := task.Loop
			if len(items) == 0 {
				items = []string{""}
			}

			anyChanged := false

			for _, item := range items {
				iterVars := play.Vars
				if item != "" {
					iterVars = mergeVars(play.Vars, map[string]string{"item": item})
				}

				label := task.Name
				if item != "" {
					label += " [item=" + item + "]"
				}

				// ### per-host when ###
				taskHosts := fresh
				if task.When != "" {
					taskHosts = nil
					for _, h := range fresh {
						hVars := mergeVars(iterVars, allFacts[hostLabel(h)])
						shouldRun, err := evalWhen(task.When, hVars)
						if err != nil {
							activeEmitter.Diagnostic(fmt.Sprintf("ERROR evaluating when: %v", err))
							totalFailed++
							shouldRun = false
						}
						if shouldRun {
							taskHosts = append(taskHosts, h)
						}
					}
				}

				activeEmitter.TaskHeader(label)

				// Emit skipped events for hosts filtered out by when.
				if task.When != "" {
					taskHostSet := make(map[string]bool, len(taskHosts))
					for _, h := range taskHosts {
						taskHostSet[hostLabel(h)] = true
					}
					for _, h := range fresh {
						if !taskHostSet[hostLabel(h)] {
							activeEmitter.TaskSkipped(hostLabel(h), "when condition false")
							stats[hostLabel(h)].skipped++
						}
					}
					if len(taskHosts) == 0 {
						continue
					}
				}

				expanded, err := expandVars(task.Params, iterVars)
				if err != nil {
					activeEmitter.Diagnostic(fmt.Sprintf("ERROR: %v", err))
					totalFailed++
					continue
				}

				factory, ok := LookupModule(task.Module)
				if !ok {
					activeEmitter.Diagnostic(fmt.Sprintf("ERROR: unknown module %q", task.Module))
					totalFailed++
					continue
				}

				mod, err := factory(expanded)
				if err != nil {
					activeEmitter.Diagnostic(fmt.Sprintf("ERROR: %v", err))
					totalFailed++
					continue
				}

				results := RunOnHosts(taskHosts, mod, parallelism, play.Become, checkMode)
				for _, r := range results {
					s := stats[r.Host]
					if s == nil {
						stats[r.Host] = &hostStats{}
						s = stats[r.Host]
						if !registered[r.Host] {
							registered[r.Host] = true
							hostOrder = append(hostOrder, r.Host)
						}
					}
					n := applyResult(r, s)
					if n > 0 {
						totalFailed += n
					}
					if r.Changed {
						anyChanged = true
					}
				}
			}

			// ### notify ###
			if anyChanged && task.Notify != "" {
				notified[task.Notify] = true
			}
		}

		// ### handlers ###
		if len(notified) > 0 {
			activeEmitter.HandlersRunning()
			for _, handler := range play.Handlers {
				if !notified[handler.Name] {
					continue
				}
				activeEmitter.HandlerHeader(handler.Name)

				expanded, err := expandVars(handler.Params, play.Vars)
				if err != nil {
					activeEmitter.Diagnostic(fmt.Sprintf("ERROR: %v", err))
					totalFailed++
					continue
				}

				factory, ok := LookupModule(handler.Module)
				if !ok {
					activeEmitter.Diagnostic(fmt.Sprintf("ERROR: unknown module %q", handler.Module))
					totalFailed++
					continue
				}

				mod, err := factory(expanded)
				if err != nil {
					activeEmitter.Diagnostic(fmt.Sprintf("ERROR: %v", err))
					totalFailed++
					continue
				}

				results := RunOnHosts(fresh, mod, parallelism, play.Become, checkMode)
				for _, r := range results {
					s := stats[r.Host]
					if s == nil {
						stats[r.Host] = &hostStats{}
						s = stats[r.Host]
					}
					totalFailed += applyResult(r, s)
				}
			}
		}
	}

	activeEmitter.RecapStarted(checkMode)
	for _, h := range hostOrder {
		s := stats[h]
		activeEmitter.HostRecap(h, s.ok, s.changed, s.failed, s.skipped)
		report.addHost(h, s)
	}

	totalOK, totalChanged := 0, 0
	for _, s := range stats {
		totalOK += s.ok
		totalChanged += s.changed
	}
	activeEmitter.RunFinished(totalOK, totalChanged, totalFailed)

	return totalFailed, report
}

// applyResult delegates per-host output to the active emitter and updates stats.
// Returns number of failures (0 or 1).
func applyResult(r Result, s *hostStats) int {
	activeEmitter.TaskResult(r)
	switch {
	case r.Err != nil:
		s.failed++
		return 1
	case r.Changed && r.DryRun:
		s.changed++
		s.ok++
	case r.Changed:
		s.changed++
		s.ok++
	default:
		s.ok++
	}
	return 0
}

func printHeader(title string) {
	const width = 70
	line := title + " "
	remaining := width - len(line)
	if remaining < 1 {
		remaining = 1
	}
	fmt.Println(line + strings.Repeat("*", remaining))
}
