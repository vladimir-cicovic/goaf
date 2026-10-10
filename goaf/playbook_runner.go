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
	Become      bool     // CLI --become forces privilege escalation on all plays
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

	lr := &listRunner{
		batch: batch, play: play, inv: inv, opts: opts, stats: stats,
		sessions: sessions, connErrs: connErrs, allFacts: allFacts,
		registeredVars: registeredVars, notified: notified,
		totalFailed: &totalFailed,
	}

	// pre_tasks, tasks, post_tasks — handlers flush after each section.
	for _, section := range [][]PlayTask{play.PreTasks, play.Tasks, play.PostTasks} {
		lr.runList(section, nil, nil, nil, false)
		lr.flushHandlers()
	}

	return totalFailed
}

// listRunner carries shared state for executing a task list.
type listRunner struct {
	batch          []Host
	play           Play
	inv            *Inventory
	opts           RunOptions
	stats          map[string]*hostStats
	sessions       map[string]*Session
	connErrs       map[string]error
	allFacts       map[string]Facts
	registeredVars map[string]map[string]string
	notified       map[string]map[string]bool
	totalFailed    *int
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
	if s := c.stats[label]; s != nil {
		s.skipped++
	} else {
		c.stats[label] = &hostStats{skipped: 1}
	}
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
				*c.totalFailed++
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

		var results []Result
		switch {
		case task.Module == "debug" || task.Module == "set_fact":
			results = c.runLocal(taskHosts, task, item)
		case task.DelegateTo != "":
			results = c.runDelegated(taskHosts, task, item)
		default:
			mkMod := func(h Host) (Module, error) {
				vars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
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
			results = runOnSessions(taskHosts, mkMod, c.sessions, c.connErrs, c.play.Become || c.opts.Become, c.opts.CheckMode)
		}

		byLabel := make(map[string]Host, len(taskHosts))
		for _, h := range taskHosts {
			byLabel[hostLabel(h)] = h
		}

		for _, r := range results {
			s := c.stats[r.Host]
			if s == nil {
				c.stats[r.Host] = &hostStats{}
				s = c.stats[r.Host]
			}
			r = applyTaskControls(r, byLabel[r.Host], task, c.inv, c.play, item, c.allFacts, c.registeredVars)
			if task.Register != "" {
				if c.registeredVars[r.Host] == nil {
					c.registeredVars[r.Host] = make(map[string]string)
				}
				c.registeredVars[r.Host][task.Register] = r.Output
			}
			n := applyResult(r, s)
			if n > 0 {
				*c.totalFailed += n
				failed[r.Host] = true
			}
			// ### notify ### — only hosts that changed without failing
			if r.Changed && r.Err == nil && task.Notify != "" {
				if c.notified[task.Notify] == nil {
					c.notified[task.Notify] = make(map[string]bool)
				}
				c.notified[task.Notify][r.Host] = true
			}
		}
	}
}

// runLocal executes control-side tasks (debug, set_fact) without SSH.
func (c *listRunner) runLocal(taskHosts []Host, task PlayTask, item string) []Result {
	results := make([]Result, 0, len(taskHosts))
	for _, h := range taskHosts {
		label := hostLabel(h)
		vars := taskVars(c.inv, c.play, h, item, c.allFacts, c.registeredVars)
		switch task.Module {
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
// Skipped in check mode, like before.
func (c *listRunner) flushHandlers() {
	if len(c.notified) == 0 || c.opts.CheckMode {
		// Still clear so re-notify works in later sections.
		c.notified = make(map[string]map[string]bool)
		return
	}
	activeEmitter.HandlersRunning()
	for _, handler := range c.play.Handlers {
		triggered := c.notified[handler.Name]
		if len(triggered) == 0 {
			continue
		}
		activeEmitter.HandlerHeader(handler.Name)

		mkMod := func(h Host) (Module, error) {
			vars := taskVars(c.inv, c.play, h, "", c.allFacts, c.registeredVars)
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
		for _, h := range c.batch {
			if triggered[hostLabel(h)] {
				handlerHosts = append(handlerHosts, h)
			}
		}
		results := runOnSessions(handlerHosts, mkMod, c.sessions, c.connErrs, c.play.Become || c.opts.Become, c.opts.CheckMode)
		for _, r := range results {
			s := c.stats[r.Host]
			if s == nil {
				c.stats[r.Host] = &hostStats{}
				s = c.stats[r.Host]
			}
			*c.totalFailed += applyResult(r, s)
		}
	}
	c.notified = make(map[string]map[string]bool)
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
