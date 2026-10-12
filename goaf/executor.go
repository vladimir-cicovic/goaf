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
	return RunOnHostsBecome(hosts, mod, parallelism, become, "", "", checkMode)
}

// RunOnHostsBecome is RunOnHosts with explicit become user/method
// (CLI --become-user/--become-method).
func RunOnHostsBecome(hosts []Host, mod Module, parallelism int, become bool, becomeUser, becomeMethod string, checkMode bool) []Result {
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

			res := runOneBecome(host, mod, become, becomeUser, becomeMethod, checkMode)

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(host)
	}

	wg.Wait()
	return results
}

func runOne(host Host, mod Module, become, checkMode bool) Result {
	return runOneBecome(host, mod, become, "", "", checkMode)
}

func runOneBecome(host Host, mod Module, become bool, becomeUser, becomeMethod string, checkMode bool) Result {
	sess, err := dialHost(host)
	if err != nil {
		return Result{Host: hostLabel(host), Err: err}
	}
	defer sess.Close()
	sess.SetBecome(become)
	sess.SetBecomeUser(becomeUser, becomeMethod)
	return RunModule(sess.DisplayHost(), sess, mod, checkMode)
}

// EnvSession wraps a Remote and prefixes environment exports to every
// command. Exports land inside the privileged shell under become, so they
// survive escalation; on WinRM the PowerShell form is used.
type EnvSession struct {
	Remote
	env map[string]string
}

// SetBecomeUser forwards to the wrapped session.
func (s EnvSession) SetBecomeUser(user, method string) {
	s.Remote.SetBecomeUser(user, method)
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

			sess, err := dialPooled(host)
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
}

// connPool keeps one connection per host for the whole run (multiplexing:
// SSH channels share the transport instead of re-handshaking per play).
// WinRM is request-based; its entries are lightweight markers.
var connPool = struct {
	sync.Mutex
	sessions map[string]Remote
	params   map[string]Host
}{sessions: map[string]Remote{}, params: map[string]Host{}}

// dialPooled returns the pooled connection for a host, dialing (and caching)
// on first use. A pooled entry is reused only when the connection parameters
// match; dead sessions are re-dialed once.
func dialPooled(h Host) (Remote, error) {
	label := hostLabel(h)
	connPool.Lock()
	sess, ok := connPool.sessions[label]
	prev, same := connPool.params[label]
	var stale Remote
	if ok && same {
		if sameHostParams(prev, h) {
			connPool.Unlock()
			if aliveSession(sess) {
				return sess, nil
			}
			stale = sess
		} else {
			// Parameters changed mid-run: drop the stale entry.
			stale = sess
			delete(connPool.sessions, label)
			delete(connPool.params, label)
			connPool.Unlock()
		}
	} else {
		connPool.Unlock()
	}
	if stale != nil {
		stale.Close()
	}
	sess, err := dialHost(h)
	if err != nil {
		return nil, err
	}
	connPool.Lock()
	connPool.sessions[label] = sess
	connPool.params[label] = h
	connPool.Unlock()
	return sess, nil
}

// sameHostParams reports whether cached connection parameters still apply.
func sameHostParams(a, b Host) bool {
	return a.Addr == b.Addr && a.Port == b.Port && a.User == b.User &&
		a.Key == b.Key && a.Connection == b.Connection &&
		a.JumpAddr == b.JumpAddr && a.JumpUser == b.JumpUser && a.JumpPort == b.JumpPort
}

// aliveSession probes an SSH transport; WinRM markers are always "alive"
// (each request dials fresh HTTP).
func aliveSession(s Remote) bool {
	ss, ok := s.(*Session)
	if !ok {
		return true
	}
	if ss.client == nil {
		return false
	}
	_, _, err := ss.client.SendRequest("keepalive@goaf", true, nil)
	return err == nil
}

// closePool closes every pooled connection (end of run).
func closePool() {
	connPool.Lock()
	defer connPool.Unlock()
	for _, sess := range connPool.sessions {
		sess.Close()
	}
	connPool.sessions = map[string]Remote{}
	connPool.params = map[string]Host{}
}

// runOnSessions executes a per-host built module on existing sessions in
// parallel. Hosts without a session report the stored connection error.
func runOnSessions(hosts []Host, mkMod func(h Host) (Module, error), sessions map[string]Remote, connErrs map[string]error, become, checkMode bool, throttle int, becomeUser, becomeMethod string) []Result {
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
			mu.Lock()
			sess, ok := sessions[label]
			mu.Unlock()
			if !ok {
				// Retry the dial: transient network blips recover here,
				// which also makes task retries useful for dead connections.
				if fresh, derr := dialPooled(host); derr == nil {
					mu.Lock()
					sessions[label] = fresh
					delete(connErrs, label)
					mu.Unlock()
					sess, ok = fresh, true
				} else {
					mu.Lock()
					err := connErrs[label]
					mu.Unlock()
					if err == nil {
						err = derr
					}
					if err == nil {
						err = fmt.Errorf("no connection to host")
					}
					mu.Lock()
					results = append(results, Result{Host: label, Err: err})
					mu.Unlock()
					return
				}
			}
			mod, err := mkMod(host)
			if err != nil {
				mu.Lock()
				results = append(results, Result{Host: label, Err: err})
				mu.Unlock()
				return
			}
			sess.SetBecome(become)
			sess.SetBecomeUser(becomeUser, becomeMethod)
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
