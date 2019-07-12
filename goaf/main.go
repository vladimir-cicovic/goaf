package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	// Pre-scan os.Args for -check/--check and -json/--json before flag.Parse()
	// so these flags work even when placed after positional arguments.
	checkMode := false
	jsonMode := false
	becomeMode := false
	filtered := os.Args[:1]
	for _, a := range os.Args[1:] {
		switch a {
		case "-check", "--check":
			checkMode = true
		case "-json", "--json":
			jsonMode = true
		case "-become", "--become":
			becomeMode = true
		default:
			filtered = append(filtered, a)
		}
	}
	os.Args = filtered

	if jsonMode {
		activeEmitter = newJSONEmitter()
	}

	invPath := flag.String("i", "inventory.yml", "path to inventory file")
	target := flag.String("t", "", "target group or host (e.g. web or 10.0.0.1:2222)")
	parallel := flag.Int("p", 10, "max number of parallel connections")
	reportPath := flag.String("report", "", "write run report to this path (.json or .html)")
	flag.Parse()

	// Was -i passed explicitly? If not, a missing default inventory is fine
	// (direct host:port targets don't need one).
	explicitInv := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "i" {
			explicitInv = true
		}
	})

	args := flag.Args()
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}

	inv, err := LoadInventory(*invPath)
	if err != nil {
		if !explicitInv && os.IsNotExist(err) {
			// No inventory file and none requested — use an empty one so that
			// direct host targets (e.g. -t example.com:22) still work.
			inv = defaultInventory()
		} else {
			fmt.Fprintf(os.Stderr, "error loading inventory: %v\n", err)
			os.Exit(1)
		}
	}

	// playbook mode: goaf [-json] [-check] -i inventory.yml run <playbook.yml>
	if args[0] == "run" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: goaf -i inventory.yml run <playbook.yml>")
			os.Exit(1)
		}
		plays, err := loadPlaybook(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading playbook: %v\n", err)
			os.Exit(1)
		}
		activeEmitter.RunStarted("playbook", 0, *parallel, checkMode)
		failed, report := RunPlaybook(plays, inv, *parallel, checkMode)
		if *reportPath != "" {
			if err := writeReport(report, *reportPath); err != nil {
				fmt.Fprintf(os.Stderr, "report error: %v\n", err)
			}
		}
		if failed > 0 {
			os.Exit(2)
		}
		return
	}

	// ad-hoc mode requires -t
	if *target == "" {
		usage()
		os.Exit(1)
	}

	hosts, err := inv.Resolve(*target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	factory, ok := LookupModule(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown action: %s\n", args[0])
		usage()
		os.Exit(1)
	}

	mod, err := factory(parseCLIParams(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "module error: %v\n", err)
		os.Exit(1)
	}

	activeEmitter.RunStarted("adhoc", len(hosts), *parallel, checkMode)
	activeEmitter.TaskHeader(args[0])

	results := RunOnHosts(hosts, mod, *parallel, becomeMode, checkMode)

	failed := 0
	changed := 0
	passed := 0
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed++
		case r.Changed:
			changed++
		default:
			passed++
		}

		if jsonMode {
			activeEmitter.TaskResult(r)
		} else {
			var status string
			switch {
			case r.Err != nil:
				status = "ERROR"
			case r.Changed && r.DryRun:
				status = "WOULD CHANGE"
			case r.Changed:
				status = "CHANGED"
			default:
				status = "OK"
			}
			fmt.Printf("[%s] %s\n", r.Host, status)
			if r.Output != "" {
				fmt.Printf("    %s\n", indent(r.Output))
			}
			if r.Err != nil {
				fmt.Printf("    %v\n", r.Err)
			}
		}
	}

	total := len(results)
	if jsonMode {
		activeEmitter.RunFinished(passed+changed, changed, failed)
	} else {
		if checkMode {
			fmt.Printf("\nCHECK MODE — PASS: %d/%d  WOULD CHANGE: %d  FAIL: %d\n", passed+changed, total, changed, failed)
		} else {
			fmt.Printf("\nPASS: %d/%d  CHANGED: %d  FAIL: %d\n", passed+changed, total, changed, failed)
		}
	}

	if *reportPath != "" {
		report := newReport("adhoc", checkMode)
		for _, r := range results {
			s := &hostStats{}
			switch {
			case r.Err != nil:
				s.failed = 1
			case r.Changed:
				s.changed = 1
				s.ok = 1
			default:
				s.ok = 1
			}
			report.addHost(r.Host, s)
		}
		if err := writeReport(report, *reportPath); err != nil {
			fmt.Fprintf(os.Stderr, "report error: %v\n", err)
		}
	}

	if failed > 0 {
		os.Exit(2)
	}
}

// parseCLIParams parses arguments as key=value pairs, with positional fallback
// for command and install/remove modules.
// Args starting with "-" are silently skipped (misplaced flags).
// KV args (containing "=") are always parsed even if non-KV args are present.
func parseCLIParams(args []string) map[string]string {
	action := args[0]
	rest := args[1:]
	params := make(map[string]string)

	var kvArgs []string
	var posArgs []string
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			continue // misplaced flag — already handled by pre-scan
		}
		if strings.Contains(a, "=") {
			kvArgs = append(kvArgs, a)
		} else {
			posArgs = append(posArgs, a)
		}
	}

	// Always parse KV args (key=value pairs)
	for _, a := range kvArgs {
		idx := strings.Index(a, "=")
		params[a[:idx]] = a[idx+1:]
	}
	if len(kvArgs) > 0 {
		return params
	}

	// Positional fallback for command, install, remove (only if no KV args)
	if len(posArgs) > 0 {
		switch action {
		case "command":
			params["cmd"] = posArgs[0]
		case "install", "package", "remove":
			params["name"] = posArgs[0]
		}
	}
	return params
}

func indent(s string) string {
	out := ""
	for i, line := range splitLines(s) {
		if i > 0 {
			out += "\n    "
		}
		out += line
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	lines = append(lines, cur)
	return lines
}

func usage() {
	fmt.Println("Usage:")
	fmt.Println("  goaf [-check] [-json] -i inventory.yml -t <group|host> <module> [params]")
	fmt.Println("  goaf [-check] [-json] -i inventory.yml run <playbook.yml>")
	fmt.Println("\nFlags:")
	fmt.Println("  -i <path>       inventory file (default: inventory.yml)")
	fmt.Println("  -t <target>     group name or host:port for ad-hoc")
	fmt.Println("  -p <n>          parallelism (default: 10)")
	fmt.Println("  -check          dry-run — show what would change, skip Apply()")
	fmt.Println("  -become         run tasks with sudo (privilege escalation)")
	fmt.Println("  -json           emit NDJSON event stream (one JSON object per line)")
	fmt.Println("  -report <path>  write run report (.json or .html)")
	fmt.Println("\nModules (ad-hoc):")
	fmt.Println("  command  \"<shell command>\"")
	fmt.Println("  install  <package>")
	fmt.Println("  remove   <package>")
	fmt.Println("  copy     src=<local> dest=<remote>")
	fmt.Println("  file     path=<path> [state=file|directory|absent] [mode=0644] [owner=root] [group=root]")
	fmt.Println("  service  name=<service> [state=started|stopped|restarted] [enabled=true|false]")
	fmt.Println("  template src=<template> dest=<remote> [key=value ...]")
	fmt.Println("  setup    (display gathered host facts: os, hostname, arch, kernel, ip)")
	fmt.Println("\nPlaybook (run):")
	fmt.Println("  goaf -i inventory.yml run site.yml")
	fmt.Println("  goaf -check -i inventory.yml run site.yml   # dry-run")
	fmt.Println("  goaf -json  -i inventory.yml run site.yml   # NDJSON output")
	fmt.Println("\nPlaybook task keys: when, loop/with_items, notify (+ top-level handlers:)")
	fmt.Println("\nExamples:")
	fmt.Println("  goaf -t web command \"uptime\"")
	fmt.Println("  goaf -t web install nginx")
	fmt.Println("  goaf -t web copy src=./nginx.conf dest=/etc/nginx/nginx.conf")
	fmt.Println("  goaf -t web file path=/var/www state=directory mode=0755")
	fmt.Println("  goaf -t web service name=nginx state=started enabled=true")
	fmt.Println("  goaf -t web template src=./nginx.conf.tmpl dest=/etc/nginx/nginx.conf port=80")
}
