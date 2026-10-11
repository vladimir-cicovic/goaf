package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// diffMode, when true, attaches unified diffs (old vs new) to changed results
// for modules that implement Differ. Set from the --diff flag.
var diffMode = false

// RunOnHosts executes a module on all hosts in parallel,
// limited to `parallelism` concurrent connections.
// When become is true, commands run with sudo privilege escalation.
// When checkMode is true, Apply is skipped (dry-run).
func RunOnHosts(hosts []Host, mod Module, parallelism int, become, checkMode bool) []Result {
	if parallelism < 1 {
		parallelism = 1
	}

	results := make([]Result, 0, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelism)

	for _, host := range hosts {
		wg.Add(1)
		go func(host Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := runOne(host, mod, become, checkMode)

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(host)
	}

	wg.Wait()
	return results
}

func runOne(host Host, mod Module, become, checkMode bool) Result {
	sess, err := dialHost(host)
	if err != nil {
		return Result{Host: hostLabel(host), Err: err}
	}
	defer sess.Close()
	sess.SetBecome(become)
	return RunModule(sess.DisplayHost(), sess, mod, checkMode)
}

// EnvSession wraps a Remote and prefixes environment exports to every
// command. Exports land inside the privileged shell under become, so they
// survive escalation; on WinRM the PowerShell form is used.
type EnvSession struct {
	Remote
	env map[string]string
}

// taskEnv merges play and task environment (task wins).
func taskEnv(play Play, task PlayTask) map[string]string {
	if len(play.Environment) == 0 && len(task.Environment) == 0 {
		return nil
	}
	out := make(map[string]string, len(play.Environment)+len(task.Environment))
	for k, v := range play.Environment {
		out[k] = v
	}
	for k, v := range task.Environment {
		out[k] = v
	}
	return out
}

// wrapEnv wraps sessions with environment exports (nil-safe passthrough).
func wrapEnv(sessions map[string]Remote, env map[string]string) map[string]Remote {
	if len(env) == 0 {
		return sessions
	}
	out := make(map[string]Remote, len(sessions))
	for k, s := range sessions {
		out[k] = EnvSession{Remote: s, env: env}
	}
	return out
}

// unwrapSession strips EnvSession wrappers to the real transport.
func unwrapSession(s Remote) Remote {
	if e, ok := s.(EnvSession); ok {
		return e.Remote
	}
	return s
}

// Run implements Remote with environment exports prefixed.
func (s EnvSession) Run(cmd string) (string, error) {
	if len(s.env) == 0 {
		return s.Remote.Run(cmd)
	}
	keys := make([]string, 0, len(s.env))
	for k := range s.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if _, ok := s.Remote.(*WinRMSession); ok {
		var b strings.Builder
		for _, k := range keys {
			b.WriteString("$env:" + k + "=" + psQuote(s.env[k]) + "; ")
		}
		return s.Remote.Run(b.String() + cmd)
	}
	var b strings.Builder
	b.WriteString("export")
	for _, k := range keys {
		b.WriteString(" " + k + "=" + shQuote(s.env[k]))
	}
	b.WriteString("; " + cmd)
	return s.Remote.Run(b.String())
}

// runLocalCommand executes a shell command on the control node and returns
// trimmed combined output (used by delegate_to: localhost).
func runLocalCommand(cmd string) (string, error) {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/c", cmd)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// connectAll opens one session per host in parallel (connection reuse:
// one connection serves facts gathering and all tasks of a play).
// Failures are returned per host label so tasks can report them.
func connectAll(hosts []Host, parallelism int) (map[string]Remote, map[string]error) {
	if parallelism < 1 {
		parallelism = 1
	}
	sessions := make(map[string]Remote, len(hosts))
	connErrs := make(map[string]error)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelism)

	for _, host := range hosts {
		wg.Add(1)
		go func(host Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			sess, err := dialHost(host)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				connErrs[hostLabel(host)] = err
				return
			}
			sessions[hostLabel(host)] = sess
		}(host)
	}

	wg.Wait()
	return sessions, connErrs
}

// closeAll closes every session in the map.
func closeAll(sessions map[string]Remote) {
	for _, sess := range sessions {
		sess.Close()
	}
}

// runOnSessions executes a per-host built module on existing sessions in
// parallel. Hosts without a session report the stored connection error.
func runOnSessions(hosts []Host, mkMod func(h Host) (Module, error), sessions map[string]Remote, connErrs map[string]error, become, checkMode bool, throttle int) []Result {
	if len(hosts) == 0 {
		return nil
	}
	results := make([]Result, 0, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	semSize := len(hosts)
	if semSize > 50 {
		semSize = 50
	}
	if throttle > 0 && throttle < semSize {
		semSize = throttle
	}
	sem := make(chan struct{}, semSize)

	for _, host := range hosts {
		wg.Add(1)
		go func(host Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			label := hostLabel(host)
			sess, ok := sessions[label]
			if !ok {
				err := connErrs[label]
				if err == nil {
					err = fmt.Errorf("no connection to host")
				}
				mu.Lock()
				results = append(results, Result{Host: label, Err: err})
				mu.Unlock()
				return
			}
			mod, err := mkMod(host)
			if err != nil {
				mu.Lock()
				results = append(results, Result{Host: label, Err: err})
				mu.Unlock()
				return
			}
			sess.SetBecome(become)
			res := RunModule(sess.DisplayHost(), sess, mod, checkMode)

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(host)
	}

	wg.Wait()
	return results
}

func hostLabel(h Host) string {
	if h.Port == 22 {
		return h.Addr
	}
	return h.Addr + ":" + itoa(h.Port)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
