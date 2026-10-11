package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// promptVars asks for play vars_prompt values (merged into play.Vars).
// Without a terminal each prompt takes its default (or empty) with a notice.
func promptVars(play *Play) {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	reader := bufio.NewReader(os.Stdin)
	for _, spec := range play.VarsPrompt {
		if !interactive {
			if spec.Default != "" && play.Vars[spec.Name] == "" {
				play.Vars[spec.Name] = spec.Default
			}
			activeEmitter.Diagnostic(fmt.Sprintf("vars_prompt %q: no terminal, using default", spec.Name))
			continue
		}
		var value string
		if spec.Private {
			pw, err := readPassword(spec.Prompt + " ")
			if err != nil {
				activeEmitter.Diagnostic(fmt.Sprintf("vars_prompt %q: %v", spec.Name, err))
				value = spec.Default
			} else {
				value = pw
			}
		} else {
			fmt.Fprintf(os.Stderr, "%s ", spec.Prompt)
			line, _ := reader.ReadString('\n')
			value = strings.TrimSpace(line)
			if value == "" {
				value = spec.Default
			}
		}
		if play.Vars == nil {
			play.Vars = map[string]string{}
		}
		play.Vars[spec.Name] = value
	}
}

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
	Become      bool     // CLI --become forces privilege escalation on all plays
	Tags        []string // run only tasks with these tags ("always" always runs)
	SkipTags    []string // never run tasks with these tags
	Limit       string   // restrict to hosts matching this group/host expression
	Serial      int      // max hosts per batch, 0 = all at once
	SerialPct   int      // ...or percent of hosts per batch (0 = unused)
	FactsTTL    int      // reuse cached facts younger than this (seconds, 0 = disabled)
	FlushCache  bool     // ignore (and refresh) cached facts
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
		// Hosts added mid-run (group_by/add_host) bypass dedup once so
		// later plays can reach them via their new groups.
		var fresh []Host
		for _, h := range hosts {
			label := hostLabel(h)
			if processed[label] && !inv.takeDynamic(label) {
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

		// Prompt for play variables (once per play, only with fresh hosts).
		if len(play.VarsPrompt) > 0 && len(fresh) > 0 {
			promptVars(&play)
		}

		if len(fresh) == 0 {
			activeEmitter.Diagnostic("(no fresh hosts — play has nothing to do)")
			continue
		}

		abortedRun := false
		lastFacts := map[string]Facts{}
		for _, batch := range splitBatches(fresh, effectiveSerial(opts, len(fresh))) {
			if abortedRun {
				break
			}
			var aborted bool
			var n int
			var facts map[string]Facts
			n, aborted, facts = runBatch(batch, play, inv, opts, stats, registeredVars)
			totalFailed += n
			abortedRun = aborted
			lastFacts = facts
		}
		if len(play.Outputs) > 0 && len(fresh) > 0 {
			printPlayOutputs(play, inv, fresh[0], lastFacts, registeredVars, report)
		}
		if abortedRun {
			break
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
// Returns failures, whether the run was aborted (any_errors_fatal /
// max_fail_percentage), and the last gathered facts (for play outputs).
func runBatch(batch []Host, play Play, inv *Inventory, opts RunOptions, stats map[string]*hostStats, registeredVars map[string]map[string]string) (int, bool, map[string]Facts) {
	totalFailed := 0

	sessions, connErrs := connectAll(batch, opts.Parallelism)
	defer closeAll(sessions)

	// Gather host facts before tasks (on the reused connections).
	var allFacts map[string]Facts
	if play.GatherFacts {
		activeEmitter.GatheringFacts()
		allFacts = gatherFactsOn(sessions, batch, opts.FactsTTL, opts.FlushCache)
		for _, h := range batch {
			activeEmitter.FactsGathered(hostLabel(h))
		}
	} else {
		allFacts = make(map[string]Facts)
	}

	notified := make(map[string]map[string]bool) // handler name -> host labels that triggered it

	lr := &listRunner{
		batch: batch, play: play, inv: inv, opts: opts, stats: stats,
		sessions: sessions, connErrs: connErrs, allFacts: allFacts,
		registeredVars: registeredVars, notified: notified,
		totalFailed: &totalFailed,
	}

	if play.Strategy == "free" {
		// Free strategy: every host runs all sections independently and in
		// parallel (no per-task barrier). Output lines may interleave.
		n, aborted := lr.runFree()
		totalFailed += n
		if aborted {
			lr.aborted = true
		}
	} else {
		// pre_tasks, tasks, post_tasks — handlers flush after each section.
		for _, section := range [][]PlayTask{play.PreTasks, play.Tasks, play.PostTasks} {
			if lr.aborted {
				break
			}
			lr.runList(section, nil, nil, nil, false)
			lr.flushHandlers()
		}
	}

	return totalFailed, lr.aborted, allFacts
}

// runFree executes all sections per host, hosts in parallel.
// Each host gets private register/notify state; stats map entries and the
// inventory are shared (guarded). Returns failures and whether any host aborted.
func (c *listRunner) runFree() (int, bool) {
	par := c.opts.Parallelism
	if par < 1 {
		par = 1
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, par)
	var mu sync.Mutex
	failed, aborted := 0, false

	for _, h := range c.batch {
		wg.Add(1)
		go func(h Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			label := hostLabel(h)
			reg := map[string]string{}
			if m, ok := c.registeredVars[label]; ok {
				for k, v := range m {
					reg[k] = v
				}
			}
			var n int
			sub := &listRunner{
				batch: []Host{h}, play: c.play, inv: c.inv, opts: c.opts,
				stats: c.stats, sessions: c.sessions, connErrs: c.connErrs,
				allFacts:       c.allFacts,
				registeredVars: map[string]map[string]string{label: reg},
				notified:       make(map[string]map[string]bool),
				totalFailed:    &n, batchFailed: map[string]bool{},
			}
			for _, section := range [][]PlayTask{c.play.PreTasks, c.play.Tasks, c.play.PostTasks} {
				if sub.aborted {
					break
				}
				sub.runList(section, nil, nil, nil, false)
				sub.flushHandlers()
			}
			mu.Lock()
			failed += n
			aborted = aborted || sub.aborted
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return failed, aborted
}

// listRunner carries shared state for executing a task list.
type listRunner struct {
	batch          []Host
	play           Play
	inv            *Inventory
	opts           RunOptions
	stats          map[string]*hostStats
	sessions       map[string]Remote
	connErrs       map[string]error
	allFacts       map[string]Facts
	registeredVars map[string]map[string]string
	notified       map[string]map[string]bool
	totalFailed    *int
	batchFailed    map[string]bool // hosts failed in this batch (abort checks)
	aborted        bool
	mu             sync.Mutex // guards stats/totalFailed (strategy: free runs hosts in parallel)
}

// statFor returns the stats entry for a label, creating it if needed.
func (c *listRunner) statFor(label string) *hostStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats[label]
	if s == nil {
		s = &hostStats{}
		c.stats[label] = s
	}
	return s
}

// addFailed adds task failures to the play total.
func (c *listRunner) addFailed(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.totalFailed += n
}

// noteFailure records an unignored failure and aborts the run when the
// play's any_errors_fatal / max_fail_percentage demands it.
func (c *listRunner) noteFailure(label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.batchFailed == nil {
		c.batchFailed = make(map[string]bool)
	}
	c.batchFailed[label] = true
	if c.aborted {
		return
	}
	if c.play.AnyErrorsFatal {
		activeEmitter.Diagnostic("aborting: any_errors_fatal set and a task failed")
		c.aborted = true
		return
	}
	if c.play.MaxFailPct != nil && len(c.batch) > 0 {
		pct := 100 * len(c.batchFailed) / len(c.batch)
		if pct > *c.play.MaxFailPct {
			activeEmitter.Diagnostic(fmt.Sprintf("aborting: %d%% failed hosts exceeds max_fail_percentage %d", pct, *c.play.MaxFailPct))
			c.aborted = true
		}
	}
}

// runList executes tasks; skip/only restrict hosts (block mechanics) and
// parentWhen ANDs an outer block condition. Returns labels failed in this list.
// When stopOnFail is true (inside blocks), hosts failing a task are skipped
// for the rest of this list; at play level it is false (goaf continues).
func (c *listRunner) runList(tasks []PlayTask, parentWhen []string, skip, only map[string]bool, stopOnFail bool) map[string]bool {
	failed := map[string]bool{}
	effSkip := skip
	if stopOnFail {
		effSkip = copySet(skip)
	}
	for _, task := range tasks {
		if c.aborted {
			return failed
		}
		if task.Module == "block" {
			c.runBlock(task, parentWhen, effSkip, only, failed)
			continue
		}
		c.runSingle(task, parentWhen, effSkip, only, failed)
		if stopOnFail {
			for h := range failed {
				effSkip[h] = true
			}
		}
	}
	return failed
}

// copySet returns a mutable copy of a label set (nil stays nil-ready).
func copySet(s map[string]bool) map[string]bool {
	out := make(map[string]bool, len(s))
	for h := range s {
		out[h] = true
	}
	return out
}

// runBlock executes block/rescue/always with per-host failure tracking:
// remaining block tasks skip failed hosts, rescue runs on failed hosts only,
// always runs on all hosts. A rescued host is removed from the failed set.
func (c *listRunner) runBlock(task PlayTask, parentWhen []string, skip, only map[string]bool, parentFailed map[string]bool) {
	if !tagsAllow(task, c.opts) {
		for _, h := range c.batch {
			if c.inScope(h, skip, only) {
				c.skipHost(h, "tagged out")
			}
		}
		return
	}
	withBlock := parentWhen
	if task.When != "" {
		withBlock = append(append([]string{}, parentWhen...), task.When)
	}
	innerFailed := c.runList(task.Block, withBlock, skip, only, true)
	if len(innerFailed) > 0 && len(task.Rescue) > 0 {
		rescueFailed := c.runList(task.Rescue, parentWhen, skip, innerFailed, false)
		for h := range innerFailed {
			if !rescueFailed[h] {
				delete(innerFailed, h) // recovered
			}
		}
		for h := range rescueFailed {
			innerFailed[h] = true
		}
	}
	if len(task.Always) > 0 {
		alwaysFailed := c.runList(task.Always, parentWhen, skip, only, false)
		for h := range alwaysFailed {
			innerFailed[h] = true
		}
	}
	for h := range innerFailed {
		parentFailed[h] = true
	}
}

// evalAll evaluates every condition; all must hold (AND).
func evalAll(conds []string, vars map[string]string) (bool, error) {
	for _, cond := range conds {
		if cond == "" {
			continue
		}
		ok, err := evalWhen(cond, vars)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

// inScope reports whether a host passes the skip/only filters.
func (c *listRunner) inScope(h Host, skip, only map[string]bool) bool {
	label := hostLabel(h)
	if skip[label] {
		return false
	}
	if len(only) > 0 && !only[label] {
		return false
	}
	return true
}

func (c *listRunner) skipHost(h Host, reason string) {
	label := hostLabel(h)
	activeEmitter.TaskSkipped(label, reason)
	c.statFor(label).skipped++
}

// evalBoth evaluates parent conditions AND the task condition for one host.
func (c *listRunner) evalBoth(h Host, vars map[string]string, parentWhen []string, taskWhen string) (bool, error) {
	if ok, err := evalAll(parentWhen, vars); err != nil || !ok {
		return ok, err
	}
	if taskWhen != "" {
		return evalWhen(taskWhen, vars)
	}
	return true, nil
}

// runSingle executes one non-block task (with loop items) on scoped hosts.
func (c *listRunner) runSingle(task PlayTask, parentWhen []string, skip, only map[string]bool, failed map[string]bool) {
	if !tagsAllow(task, c.opts) {
		for _, h := range c.batch {
			if c.inScope(h, skip, only) {
				c.skipHost(h, "tagged out")
			}
		}
		return
	}

	items := task.Loop
	if len(items) == 0 {
		items = []string{""}
	}

	for _, item := range items {
		label := task.Name
		if item != "" {
			label += " [item=" + item + "]"
		}

		// ### per-host when (parent AND task) ###
		var taskHosts []Host
		for _, h := range c.batch {
			if !c.inScope(h, skip, only) {
				continue
			}
			hVars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
			shouldRun, err := c.evalBoth(h, hVars, parentWhen, task.When)
			if err != nil {
				activeEmitter.Diagnostic(fmt.Sprintf("ERROR evaluating when: %v", err))
				c.addFailed(1)
				shouldRun = false
			}
			if shouldRun {
				taskHosts = append(taskHosts, h)
			}
		}

		activeEmitter.TaskHeader(label)

		// Emit skipped events for scoped-out hosts.
		scoped := make(map[string]bool, len(taskHosts))
		for _, h := range taskHosts {
			scoped[hostLabel(h)] = true
		}
		for _, h := range c.batch {
			if c.inScope(h, skip, only) && !scoped[hostLabel(h)] {
				c.skipHost(h, "when condition false")
			}
		}
		if len(taskHosts) == 0 {
			continue
		}

		// ### run_once — first host only ###
		if task.RunOnce && len(taskHosts) > 1 {
			for _, h := range taskHosts[1:] {
				c.skipHost(h, "run_once (ran on "+hostLabel(taskHosts[0])+")")
			}
			taskHosts = taskHosts[:1]
		}

		// ### meta tasks (flush_handlers) — control-side, no per-host run ###
		if task.Module == "meta" {
			c.runMeta(task)
			continue
		}

		byLabel := make(map[string]Host, len(taskHosts))
		for _, h := range taskHosts {
			byLabel[hostLabel(h)] = h
		}

		// ### retries: 1 + task.Retries attempts, `until` decides success ###
		pending := taskHosts
		final := make(map[string]Result, len(taskHosts))
		for attempt := 0; ; attempt++ {
			if len(pending) == 0 {
				break
			}
			for _, r := range c.executeItem(pending, task, item) {
				h := byLabel[r.Host]
				r = applyTaskControls(r, h, task, c.inv, c.play, item, c.allFacts, c.registeredVars)
				final[r.Host] = r
			}
			var next []Host
			for _, h := range pending {
				if c.needsRetry(final[hostLabel(h)], task, h, item) && attempt < task.Retries {
					next = append(next, h)
				}
			}
			if len(next) == 0 {
				break
			}
			if task.Delay > 0 {
				time.Sleep(time.Duration(task.Delay) * time.Second)
			}
			pending = next
		}

		for _, h := range taskHosts {
			r := final[hostLabel(h)]
			s := c.statFor(r.Host)
			if task.Register != "" {
				if c.registeredVars[r.Host] == nil {
					c.registeredVars[r.Host] = make(map[string]string)
				}
				c.registeredVars[r.Host][task.Register] = r.Output
			}
			n := applyResult(r, s)
			if n > 0 {
				c.addFailed(n)
				failed[r.Host] = true
				c.noteFailure(r.Host)
			}
			// Record applied remote changes for drift detection.
			// Always-run modules (no idempotent desired state) are skipped:
			// their Check is trivially true, so drift would flag them forever.
			switch task.Module {
			case "debug", "set_fact", "meta", "command", "upgrade", "reboot", "script", "setup":
			default:
				if r.Changed && r.Err == nil && !c.opts.CheckMode && task.DelegateTo == "" {
					if expanded, err := c.expandTask(task, h, item); err == nil {
						recordState(h, c.play.Name, task.Module, expanded, r.Output)
					}
				}
			}
			// ### notify ### — only hosts that changed without failing
			if r.Changed && r.Err == nil && task.Notify != "" {
				if c.notified[task.Notify] == nil {
					c.notified[task.Notify] = make(map[string]bool)
				}
				c.notified[task.Notify][r.Host] = true
			}
		}
		if c.aborted {
			return
		}
	}
}

// executeItem runs one attempt of a task on the given hosts (no controls).
func (c *listRunner) executeItem(taskHosts []Host, task PlayTask, item string) []Result {
	switch {
	case task.Module == "debug" || task.Module == "set_fact" || task.Module == "assert" ||
		task.Module == "group_by" || task.Module == "add_host":
		return c.runLocal(taskHosts, task, item)
	case task.DelegateTo != "":
		return c.runDelegated(taskHosts, task, item)
	default:
		mkMod := func(h Host) (Module, error) {
			return c.makeModule(task, h, item)
		}
		return runOnSessions(taskHosts, mkMod, c.sessions, c.connErrs, c.play.Become || c.opts.Become, c.opts.CheckMode, task.Throttle)
	}
}

// needsRetry reports whether a judged result needs another attempt:
// unignored failure, or an unmet `until` condition.
func (c *listRunner) needsRetry(r Result, task PlayTask, h Host, item string) bool {
	if r.Err != nil && !r.Ignored {
		if task.Until == "" {
			return true
		}
	}
	if task.Until == "" {
		return false
	}
	vars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
	vars["result"] = r.Output
	if r.Changed {
		vars["changed"] = "true"
	} else {
		vars["changed"] = "false"
	}
	ok, err := evalWhen(task.Until, vars)
	if err != nil {
		activeEmitter.Diagnostic(fmt.Sprintf("ERROR evaluating until: %v", err))
		return false
	}
	return !ok
}

// runMeta executes a meta task (currently only flush_handlers).
func (c *listRunner) runMeta(task PlayTask) {
	action, _ := task.Params["action"]
	if action == "" {
		action, _ = task.Params["cmd"]
	}
	switch action {
	case "flush_handlers":
		n := c.flushHandlers()
		activeEmitter.TaskResult(Result{Host: "localhost", Output: fmt.Sprintf("flushed %d handler(s)", n)})
	default:
		activeEmitter.TaskResult(Result{Host: "localhost",
			Err: fmt.Errorf("unknown meta action %q (want flush_handlers)", action)})
		c.addFailed(1)
	}
}

// runLocal executes control-side tasks (debug, set_fact, assert, group_by,
// add_host) without SSH.
func (c *listRunner) runLocal(taskHosts []Host, task PlayTask, item string) []Result {
	results := make([]Result, 0, len(taskHosts))
	for _, h := range taskHosts {
		label := hostLabel(h)
		vars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
		switch task.Module {
		case "assert":
			results = append(results, runAssert(label, task, vars))
		case "group_by":
			results = append(results, c.runGroupBy(label, h, task, vars))
		case "add_host":
			results = append(results, c.runAddHost(label, task, vars))
		case "debug":
			results = append(results, runDebug(label, task, vars))
		case "set_fact":
			expanded, err := expandVars(task.Params, vars)
			if err != nil {
				results = append(results, Result{Host: label, Err: err})
				continue
			}
			if c.registeredVars[label] == nil {
				c.registeredVars[label] = make(map[string]string)
			}
			keys := make([]string, 0, len(expanded))
			for k, v := range expanded {
				c.registeredVars[label][k] = v
				keys = append(keys, k+"="+v)
			}
			results = append(results, Result{Host: label, Output: "set: " + strings.Join(keys, ", ")})
		}
	}
	return results
}

// runAssert evaluates `that` conditions (that, that.0, that.1, ... sorted)
// with full task vars. Failure carries fail_msg (or a default); success
// carries success_msg (or a pass summary).
func runAssert(label string, task PlayTask, vars map[string]string) Result {
	var conds []string
	if t, ok := task.Params["that"]; ok && t != "" {
		conds = append(conds, t)
	}
	var numbered []string
	for k := range task.Params {
		if strings.HasPrefix(k, "that.") {
			numbered = append(numbered, k)
		}
	}
	sort.Strings(numbered)
	for _, k := range numbered {
		conds = append(conds, task.Params[k])
	}
	if len(conds) == 0 {
		return Result{Host: label, Err: fmt.Errorf("assert needs 'that'")}
	}
	for _, cond := range conds {
		ok, err := evalWhen(cond, vars)
		if err != nil {
			return Result{Host: label, Err: fmt.Errorf("assert: %w", err)}
		}
		if !ok {
			msg := task.Params["fail_msg"]
			if msg == "" {
				msg = fmt.Sprintf("assertion failed: %s", cond)
			} else if expanded, err := expandVars(map[string]string{"m": msg}, vars); err == nil {
				msg = expanded["m"]
			}
			return Result{Host: label, Err: fmt.Errorf("%s", msg)}
		}
	}
	msg := task.Params["success_msg"]
	if msg == "" {
		msg = fmt.Sprintf("all %d assertion(s) passed", len(conds))
	} else if expanded, err := expandVars(map[string]string{"m": msg}, vars); err == nil {
		msg = expanded["m"]
	}
	return Result{Host: label, Output: msg}
}

// runGroupBy adds the host to a computed group (key=value template).
func (c *listRunner) runGroupBy(label string, h Host, task PlayTask, vars map[string]string) Result {
	keyTmpl, ok := task.Params["key"]
	if !ok || keyTmpl == "" {
		return Result{Host: label, Err: fmt.Errorf("group_by needs 'key' (key=groupname or key=group_{{...}})")}
	}
	expanded, err := expandVars(map[string]string{"k": keyTmpl}, vars)
	if err != nil {
		return Result{Host: label, Err: err}
	}
	kv := expanded["k"]
	// Ansible form "key=<group>" or a plain group name/template.
	group := kv
	if strings.HasPrefix(kv, "key=") {
		group = strings.TrimPrefix(kv, "key=")
	}
	if group == "" {
		return Result{Host: label, Err: fmt.Errorf("group_by key expanded to empty, got %q", kv)}
	}
	raw := h.Addr
	if h.Port != 0 && h.Port != 22 {
		raw = h.Addr + ":" + strconv.Itoa(h.Port)
	}
	c.inv.mu.Lock()
	defer c.inv.mu.Unlock()
	g := c.inv.Groups[group]
	for _, existing := range g.Hosts {
		if existing == raw {
			c.inv.markDynamicLocked(label)
			return Result{Host: label, Output: "already in group " + group}
		}
	}
	g.Hosts = append(g.Hosts, raw)
	c.inv.Groups[group] = g
	c.inv.markDynamicLocked(label)
	return Result{Host: label, Output: "added to group " + group}
}

// runAddHost adds a brand-new host (with vars) to groups.
func (c *listRunner) runAddHost(label string, task PlayTask, vars map[string]string) Result {
	expanded, err := expandVars(task.Params, vars)
	if err != nil {
		return Result{Host: label, Err: err}
	}
	name, ok := expanded["name"]
	if !ok || name == "" {
		return Result{Host: label, Err: fmt.Errorf("add_host needs 'name'")}
	}
	entry := HostEntry{Vars: map[string]string{}}
	h := Host{Addr: name, Port: 22}
	for k, v := range expanded {
		switch k {
		case "name":
		case "groups":
		case "user":
			h.User = v
		case "port":
			if n, err := strconv.Atoi(v); err == nil {
				h.Port = n
			}
		case "key":
			h.Key = v
		case "connection":
			h.Connection = v
		case "password":
			h.Password = v
		default:
			entry.Vars[k] = v
		}
	}
	if h.Connection == "" {
		h.Connection = "ssh"
	}
	groups := []string{}
	for _, g := range strings.Split(expanded["groups"], ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return Result{Host: label, Err: fmt.Errorf("add_host needs 'groups'")}
	}
	c.inv.mu.Lock()
	defer c.inv.mu.Unlock()
	if c.inv.Groups == nil {
		c.inv.Groups = map[string]Group{}
	}
	for _, gname := range groups {
		g := c.inv.Groups[gname]
		found := false
		for _, existing := range g.Hosts {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			g.Hosts = append(g.Hosts, name)
		}
		c.inv.Groups[gname] = g
	}
	if c.inv.Hosts == nil {
		c.inv.Hosts = map[string]HostEntry{}
	}
	c.inv.Hosts[name] = entry
	// Brand-new hosts are always reachable later (never pre-processed),
	// but flag them too: a re-added known host bypasses dedup once.
	c.inv.markDynamicLocked(name)
	return Result{Host: label, Output: "added host " + name}
}

// runDebug renders a debug task: var=<name> prints one variable,
// msg=<text> prints expanded text.
func runDebug(label string, task PlayTask, vars map[string]string) Result {
	if v, ok := task.Params["var"]; ok {
		return Result{Host: label, Output: v + ": " + vars[v]}
	}
	msg, ok := task.Params["msg"]
	if !ok {
		return Result{Host: label, Err: fmt.Errorf("debug needs 'var' or 'msg'")}
	}
	expanded, err := expandVars(map[string]string{"msg": msg}, vars)
	if err != nil {
		return Result{Host: label, Err: err}
	}
	return Result{Host: label, Output: expanded["msg"]}
}

// runDelegated runs a command task on the control node (delegate_to:
// localhost). Other modules are rejected — they need a remote session.
func (c *listRunner) runDelegated(taskHosts []Host, task PlayTask, item string) []Result {
	if task.DelegateTo != "localhost" && task.DelegateTo != "127.0.0.1" {
		results := make([]Result, 0, len(taskHosts))
		for _, h := range taskHosts {
			results = append(results, Result{Host: hostLabel(h),
				Err: fmt.Errorf("delegate_to supports only localhost, got %q", task.DelegateTo)})
		}
		return results
	}
	if task.Module != "command" {
		results := make([]Result, 0, len(taskHosts))
		for _, h := range taskHosts {
			results = append(results, Result{Host: hostLabel(h),
				Err: fmt.Errorf("delegate_to: localhost supports the command module only, got %q", task.Module)})
		}
		return results
	}
	results := make([]Result, 0, len(taskHosts))
	for _, h := range taskHosts {
		label := hostLabel(h)
		vars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
		expanded, err := expandVars(task.Params, vars)
		if err != nil {
			results = append(results, Result{Host: label, Err: err})
			continue
		}
		cmd, ok := expanded["cmd"]
		if !ok {
			results = append(results, Result{Host: label, Err: fmt.Errorf("command needs 'cmd'")})
			continue
		}
		out, err := runLocalCommand(cmd)
		if err != nil {
			results = append(results, Result{Host: label, Output: out, Err: err})
			continue
		}
		results = append(results, Result{Host: label, Changed: true, Output: out})
	}
	return results
}

// flushHandlers runs notified handlers (per-host) and clears them.
// Skipped in check mode, like before. Returns handlers executed.
func (c *listRunner) flushHandlers() int {
	if len(c.notified) == 0 || c.opts.CheckMode {
		// Still clear so re-notify works in later sections.
		c.notified = make(map[string]map[string]bool)
		return 0
	}
	ran := 0
	activeEmitter.HandlersRunning()
	for _, handler := range c.play.Handlers {
		triggered := c.notified[handler.Name]
		if len(triggered) == 0 {
			continue
		}
		activeEmitter.HandlerHeader(handler.Name)

		mkMod := func(h Host) (Module, error) {
			return c.makeModule(handler, h, "")
		}

		var handlerHosts []Host
		for _, h := range c.batch {
			if triggered[hostLabel(h)] {
				handlerHosts = append(handlerHosts, h)
			}
		}
		results := runOnSessions(handlerHosts, mkMod, c.sessions, c.connErrs, c.play.Become || c.opts.Become, c.opts.CheckMode, 0)
		for _, r := range results {
			s := c.statFor(r.Host)
			c.addFailed(applyResult(r, s))
			ran++
		}
	}
	c.notified = make(map[string]map[string]bool)
	return ran
}

// makeModule expands task params for one host and builds the module,
// resolving role file paths (files//templates/) for role tasks.
func (c *listRunner) makeModule(task PlayTask, h Host, item string) (Module, error) {
	expanded, err := c.expandTask(task, h, item)
	if err != nil {
		return nil, err
	}
	factory, ok := LookupModule(task.Module)
	if !ok {
		return nil, fmt.Errorf("unknown module %q", task.Module)
	}
	return factory(expanded)
}

// expandTask expands task params for one host (role file resolution included).
func (c *listRunner) expandTask(task PlayTask, h Host, item string) (map[string]string, error) {
	vars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
	expanded, err := expandVars(task.Params, vars)
	if err != nil {
		return nil, err
	}
	if task.RoleDir != "" {
		switch task.Module {
		case "copy", "template", "script":
			if src, ok := expanded["src"]; ok {
				expanded["src"] = resolveRoleFile(task.RoleDir, task.Module, src)
			}
		}
	}
	return expanded, nil
}

// printPlayOutputs evaluates play `output` values with the first host's
// vars and prints them (also stored in the report).
func printPlayOutputs(play Play, inv *Inventory, h Host, facts map[string]Facts, registeredVars map[string]map[string]string, report *RunReport) {
	vars := taskVars(inv, play, h, "", facts, registeredVars)
	for name, tmpl := range play.Outputs {
		expanded, err := expandVars(map[string]string{"v": tmpl}, vars)
		if err != nil {
			activeEmitter.Diagnostic(fmt.Sprintf("ERROR in output %q: %v", name, err))
			continue
		}
		activeEmitter.PlayOutput(name, expanded["v"])
		if report.Outputs == nil {
			report.Outputs = make(map[string]string)
		}
		report.Outputs[name] = expanded["v"]
	}
}

// taskVars merges variable layers for one host and task iteration:
// inventory (group, then host) < workspace < play vars < facts < loop item
// < registered.
func taskVars(inv *Inventory, play Play, h Host, item string, facts map[string]Facts, registeredVars map[string]map[string]string) map[string]string {
	vars := inv.varsForHost(h)
	for k, v := range workspaceVars {
		vars[k] = v
	}
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

// effectiveSerial resolves Serial/SerialPct to a batch size for n hosts.
func effectiveSerial(opts RunOptions, n int) int {
	if opts.SerialPct > 0 && n > 0 {
		if pct := n * opts.SerialPct / 100; pct >= 1 {
			return pct
		}
		return 1
	}
	return opts.Serial
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
// Fresh-enough cache entries (ttlSec, 0 = disabled) are reused instead of SSH;
// flush drops the cache first.
func gatherFactsOn(sessions map[string]Remote, hosts []Host, ttlSec int, flush bool) map[string]Facts {
	if flush {
		clearFactsCache()
	}
	result := make(map[string]Facts, len(hosts))
	var mu sync.Mutex
	var cache map[string]cachedFacts
	var cacheMu sync.Mutex
	if ttlSec > 0 {
		cache = loadFactsCache()
	}
	now := time.Now().Unix()
	var wg sync.WaitGroup
	semSize := len(hosts)
	if semSize > 50 {
		semSize = 50
	}
	if semSize < 1 {
		semSize = 1
	}
	sem := make(chan struct{}, semSize)
	dirty := false

	for _, host := range hosts {
		wg.Add(1)
		go func(h Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			label := hostLabel(h)
			if ttlSec > 0 {
				cacheMu.Lock()
				ce, ok := cache[label]
				cacheMu.Unlock()
				if ok && now-ce.GatheredAt < int64(ttlSec) && len(ce.Facts) > 0 {
					cacheMu.Lock()
					result[label] = ce.Facts
					cacheMu.Unlock()
					return
				}
			}
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
			if ttlSec > 0 && len(facts) > 0 {
				cacheMu.Lock()
				cache[label] = cachedFacts{Facts: facts, GatheredAt: now}
				dirty = true
				cacheMu.Unlock()
			}
		}(host)
	}
	wg.Wait()
	if dirty {
		saveFactsCache(cache)
	}
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
