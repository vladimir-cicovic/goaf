package main

import (
	"fmt"
	"sync"
)

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
	sess, err := Connect(host)
	if err != nil {
		return Result{Host: hostLabel(host), Err: err}
	}
	defer sess.Close()
	sess.Become = become
	return RunModule(sess.Host, sess, mod, checkMode)
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
