package main

import (
	"fmt"
	"os"
	"sync"
)

// runDrift re-checks recorded state against live hosts: for every managed
// piece (host + module + params) it runs Check and reports whether the host
// is still in the desired state (OK), drifted (DRIFT), or errored.
// Returns 0 when clean, 2 when drift/errors were found.
func runDrift(invPath string, become bool, parallelism int) int {
	inv, err := LoadInventory(invPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading inventory: %v\n", err)
		return 1
	}
	entries := loadState()
	if len(entries) == 0 {
		fmt.Println("no recorded state yet — run something first")
		return 0
	}

	// Index current inventory hosts by label (prefer live connection data).
	invHosts := map[string]Host{}
	if inv.Groups != nil {
		for name := range inv.Groups {
			hosts, err := inv.Resolve(name)
			if err != nil {
				continue
			}
			for _, h := range hosts {
				invHosts[hostLabel(h)] = h
			}
		}
	}

	// Group entries per host (state file already dedupes by fingerprint).
	byHost := map[string][]stateEntry{}
	var order []string
	for _, e := range entries {
		if _, ok := byHost[e.Host]; !ok {
			order = append(order, e.Host)
		}
		byHost[e.Host] = append(byHost[e.Host], e)
	}

	if parallelism < 1 {
		parallelism = 10
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelism)
	okCount, driftCount, errCount := 0, 0, 0

	for _, label := range order {
		wg.Add(1)
		go func(label string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			list := byHost[label]
			h, ok := invHosts[label]
			if !ok {
				// Fall back to stored connection data.
				h = Host{Addr: list[0].Addr, Port: list[0].Port, User: list[0].User, Key: list[0].Key}
			}
			sess, err := Connect(h)
			if err != nil {
				mu.Lock()
				fmt.Printf("%-28s UNREACHABLE: %v\n", label, err)
				errCount += len(list)
				mu.Unlock()
				return
			}
			defer sess.Close()
			sess.Become = become

			for _, e := range list {
				factory, ok := LookupModule(e.Module)
				if !ok {
					mu.Lock()
					fmt.Printf("%-28s ERROR: unknown module %q\n", label, e.Module)
					errCount++
					mu.Unlock()
					continue
				}
				mod, err := factory(e.Params)
				if err != nil {
					mu.Lock()
					fmt.Printf("%-28s ERROR: %v\n", label, err)
					errCount++
					mu.Unlock()
					continue
				}
				needed, err := mod.Check(sess)
				mu.Lock()
				switch {
				case err != nil:
					fmt.Printf("%-28s ERROR: %s: %v\n", label, describeEntry(e), err)
					errCount++
				case needed:
					fmt.Printf("%-28s DRIFT: %s\n", label, describeEntry(e))
					driftCount++
				default:
					fmt.Printf("%-28s OK: %s\n", label, describeEntry(e))
					okCount++
				}
				mu.Unlock()
			}
		}(label)
	}
	wg.Wait()

	fmt.Printf("drift: %d ok, %d drifted, %d errors\n", okCount, driftCount, errCount)
	if driftCount > 0 || errCount > 0 {
		return 2
	}
	return 0
}

// describeEntry renders one state entry for humans.
func describeEntry(e stateEntry) string {
	switch e.Module {
	case "command":
		return "command"
	case "install", "remove", "package":
		return e.Module + " " + e.Params["name"]
	case "copy", "template", "fetch":
		return e.Module + " " + e.Params["dest"]
	case "file", "lineinfile":
		return e.Module + " " + e.Params["path"]
	case "service":
		return e.Module + " " + e.Params["name"]
	case "user":
		return e.Module + " " + e.Params["name"]
	default:
		return e.Module
	}
}
