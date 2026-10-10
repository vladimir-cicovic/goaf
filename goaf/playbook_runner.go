package main

import (
	"fmt"
	"strings"
	"sync"
)

type hostStats struct {
	ok      int
	changed int
	failed  int
	skipped int // plays skipped — host already processed in a previous play
}

// RunOptions controls playbook execution.
type RunOptions struct {
	Parallelism int
	CheckMode   bool
	Tags        []string // run only tasks with these tags ("always" always runs)
	SkipTags    []string // never run tasks with these tags
	Limit       string   // restrict to hosts matching this group/host expression
	Serial      int      // max hosts per batch, 0 = all at once
}

// RunPlaybook executes all plays in a playbook and prints Ansible-style output.
// Each host is processed only in the first play it appears in; subsequent plays
// that target the same host skip it (cross-play deduplication).
// In checkMode, Apply is never called — tasks report "would change" instead.
// Returns the number of failed tasks and a structured RunReport.
func RunPlaybook(plays []Play, inv *Inventory, parallelism int, checkMode bool) (int, *RunReport) {
	return RunPlaybookOpts(plays, inv, RunOptions{Parallelism: parallelism, CheckMode: checkMode})
}

// RunPlaybookOpts is RunPlaybook with full run options (tags/limit/serial).
func RunPlaybookOpts(plays []Play, inv *Inventory, opts RunOptions) (int, *RunReport) {
	if opts.Parallelism < 1 {
		opts.Parallelism = 1
	}
	report := newReport("playbook", opts.CheckMode)
	stats := make(map[string]*hostStats)
	var hostOrder []string
	registered := make(map[string]bool)
	processed := make(map[string]bool)
	registeredVars := make(map[string]map[string]string) // per-host register vars, whole run

	totalFailed := 0

	limitSet := map[string]bool{}
	if opts.Limit != "" {
		limHosts, err := inv.Resolve(opts.Limit)
		if err != nil {
			activeEmitter.Diagnostic(fmt.Sprintf("ERROR resolving limit %q: %v", opts.Limit, err))
			return 1, report
		}
		for _, h := range limHosts {
			limitSet[hostLabel(h)] = true
		}
	}

	for i, play := range plays {
		activeEmitter.PlayStarted(play.Name, i == 0)

		hosts, err := inv.Resolve(play.Hosts)
		if err != nil {
			activeEmitter.Diagnostic(fmt.Sprintf("ERROR resolving hosts %q: %v", play.Hosts, err))
			totalFailed++
			continue
		}
		if len(limitSet) > 0 {
			filtered := hosts[:0]
			for _, h := range hosts {
				if limitSet[hostLabel(h)] {
					filtered = append(filtered, h)
				}
			}
			hosts = filtered
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

		for _, batch := range splitBatches(fresh, opts.Serial) {
			totalFailed += runBatch(batch, play, inv, opts, stats, registeredVars)
		}
	}

	activeEmitter.RecapStarted(opts.CheckMode)
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

// runBatch connects one batch of hosts once (connection reuse), gathers facts
// on those connections, runs all tasks and handlers, then closes everything.
func runBatch(batch []Host, play Play, inv *Inventory, opts RunOptions, stats map[string]*hostStats, registeredVars map[string]map[string]string) int {
	totalFailed := 0

	sessions, connErrs := connectAll(batch, opts.Parallelism)
	defer closeAll(sessions)

	// Gather host facts before tasks (on the reused connections).
	var allFacts map[string]Facts
	if play.GatherFacts {
		activeEmitter.GatheringFacts()
		allFacts = gatherFactsOn(sessions, batch)
		for _, h := range batch {
			activeEmitter.FactsGathered(hostLabel(h))
		}
	} else {
		allFacts = make(map[string]Facts)
	}

	notified := make(map[string]map[string]bool) // handler name -> host labels that triggered it

	for _, task := range play.Tasks {
		if !tagsAllow(task, opts) {
			for _, h := range batch {
				label := hostLabel(h)
				activeEmitter.TaskSkipped(label, "tagged out")
				stats[label].skipped++
			}
			continue
		}

		// ### loop / single ###
		items := task.Loop
		if len(items) == 0 {
			items = []string{""}
		}

		for _, item := range items {
			label := task.Name
			if item != "" {
				label += " [item=" + item + "]"
			}

			// ### per-host when ###
			taskHosts := batch
			if task.When != "" {
				taskHosts = nil
				for _, h := range batch {
					hVars := taskVars(inv, play, h, item, allFacts, registeredVars)
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
				for _, h := range batch {
					if !taskHostSet[hostLabel(h)] {
						activeEmitter.TaskSkipped(hostLabel(h), "when condition false")
						stats[hostLabel(h)].skipped++
					}
				}
				if len(taskHosts) == 0 {
					continue
				}
			}

			mkMod := func(h Host) (Module, error) {
				vars := taskVars(inv, play, h, item, allFacts, registeredVars)
				expanded, err := expandVars(task.Params, vars)
				if err != nil {
					return nil, err
				}
				factory, ok := LookupModule(task.Module)
				if !ok {
					return nil, fmt.Errorf("unknown module %q", task.Module)
				}
				return factory(expanded)
			}

			byLabel := make(map[string]Host, len(taskHosts))
			for _, h := range taskHosts {
				byLabel[hostLabel(h)] = h
			}

			results := runOnSessions(taskHosts, mkMod, sessions, connErrs, play.Become, opts.CheckMode)
			for _, r := range results {
				s := stats[r.Host]
				if s == nil {
					stats[r.Host] = &hostStats{}
					s = stats[r.Host]
				}
				r = applyTaskControls(r, byLabel[r.Host], task, inv, play, item, allFacts, registeredVars)
				if task.Register != "" {
					if registeredVars[r.Host] == nil {
						registeredVars[r.Host] = make(map[string]string)
					}
					registeredVars[r.Host][task.Register] = r.Output
				}
				n := applyResult(r, s)
				if n > 0 {
					totalFailed += n
				}
				// ### notify ### — only hosts that changed without failing
				if r.Changed && r.Err == nil && task.Notify != "" {
					if notified[task.Notify] == nil {
						notified[task.Notify] = make(map[string]bool)
					}
					notified[task.Notify][r.Host] = true
				}
			}
		}
	}

	// ### handlers ### — run only on notified hosts (never in check mode)
	if len(notified) > 0 && !opts.CheckMode {
		activeEmitter.HandlersRunning()
		for _, handler := range play.Handlers {
			triggered := notified[handler.Name]
			if len(triggered) == 0 {
				continue
			}
			activeEmitter.HandlerHeader(handler.Name)

			mkMod := func(h Host) (Module, error) {
				vars := taskVars(inv, play, h, "", allFacts, registeredVars)
				expanded, err := expandVars(handler.Params, vars)
				if err != nil {
					return nil, err
				}
				factory, ok := LookupModule(handler.Module)
				if !ok {
					return nil, fmt.Errorf("unknown module %q", handler.Module)
				}
				return factory(expanded)
			}

			var handlerHosts []Host
			for _, h := range batch {
				if triggered[hostLabel(h)] {
					handlerHosts = append(handlerHosts, h)
				}
			}
			results := runOnSessions(handlerHosts, mkMod, sessions, connErrs, play.Become, opts.CheckMode)
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

	return totalFailed
}

// taskVars merges variable layers for one host and task iteration:
// inventory (group, then host) < play vars < facts < loop item < registered.
func taskVars(inv *Inventory, play Play, h Host, item string, facts map[string]Facts, registeredVars map[string]map[string]string) map[string]string {
	vars := inv.varsForHost(h)
	for k, v := range play.Vars {
		vars[k] = v
	}
	if f, ok := facts[hostLabel(h)]; ok {
		for k, v := range f {
			vars[k] = v
		}
	}
	if item != "" {
		vars["item"] = item
	}
	for k, v := range registeredVars[hostLabel(h)] {
		vars[k] = v
	}
	return vars
}

// applyTaskControls evaluates changed_when / failed_when / ignore_errors and
// records registered output. The error is kept on ignored failures so the
// emitter can show what was ignored.
func applyTaskControls(r Result, h Host, task PlayTask, inv *Inventory, play Play, item string, facts map[string]Facts, registeredVars map[string]map[string]string) Result {
	vars := taskVars(inv, play, h, item, facts, registeredVars)
	vars["result"] = r.Output
	if r.Changed {
		vars["changed"] = "true"
	} else {
		vars["changed"] = "false"
	}
	if r.Err == nil && task.ChangedWhen != "" {
		ok, err := evalWhen(task.ChangedWhen, vars)
		if err != nil {
			r.Err = fmt.Errorf("evaluating changed_when: %w", err)
		} else {
			r.Changed = ok
		}
	}
	if task.FailedWhen != "" {
		ok, err := evalWhen(task.FailedWhen, vars)
		if err != nil {
			r.Err = fmt.Errorf("evaluating failed_when: %w", err)
		} else if ok {
			r.Err = fmt.Errorf("failed_when condition met")
		}
	}
	if r.Err != nil && task.IgnoreErrs {
		r.Ignored = true
	}
	return r
}

// tagsAllow reports whether a task passes the --tags/--skip-tags filter.
// Tasks tagged "always" run unless explicitly skipped.
func tagsAllow(task PlayTask, opts RunOptions) bool {
	if len(opts.SkipTags) > 0 && intersects(task.Tags, opts.SkipTags) {
		return false
	}
	if len(opts.Tags) == 0 {
		return true
	}
	if intersects(task.Tags, opts.Tags) {
		return true
	}
	for _, t := range task.Tags {
		if t == "always" {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// splitBatches splits hosts into batches of at most serial hosts.
func splitBatches(hosts []Host, serial int) [][]Host {
	if serial <= 0 || serial >= len(hosts) {
		return [][]Host{hosts}
	}
	var out [][]Host
	for i := 0; i < len(hosts); i += serial {
		end := i + serial
		if end > len(hosts) {
			end = len(hosts)
		}
		out = append(out, hosts[i:end])
	}
	return out
}

// gatherFactsOn collects facts over existing sessions in parallel.
// Hosts without a session receive an empty Facts map so tasks can still run.
func gatherFactsOn(sessions map[string]*Session, hosts []Host) map[string]Facts {
	result := make(map[string]Facts, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	semSize := len(hosts)
	if semSize > 50 {
		semSize = 50
	}
	if semSize < 1 {
		semSize = 1
	}
	sem := make(chan struct{}, semSize)

	for _, host := range hosts {
		wg.Add(1)
		go func(h Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			label := hostLabel(h)
			mu.Lock()
			sess, ok := sessions[label]
			mu.Unlock()
			facts := Facts{}
			if ok {
				facts = gatherOne(sess)
			}
			mu.Lock()
			result[label] = facts
			mu.Unlock()
		}(host)
	}
	wg.Wait()
	return result
}

// applyResult delegates per-host output to the active emitter and updates stats.
// Returns number of failures (0 or 1). Ignored failures count as ok.
func applyResult(r Result, s *hostStats) int {
	activeEmitter.TaskResult(r)
	switch {
	case r.Err != nil && !r.Ignored:
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
