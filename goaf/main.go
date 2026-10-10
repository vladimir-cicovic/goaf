package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// splitFlag parses "-name", "--name" and "--name=value" (also single dash).
func splitFlag(a string) (name, val string, hasVal bool) {
	t := strings.TrimLeft(a, "-")
	if t == "" || a == t {
		return "", "", false // not a flag at all
	}
	if i := strings.Index(t, "="); i >= 0 {
		return t[:i], t[i+1:], true
	}
	return t, "", false
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	// Pre-scan os.Args for flags before flag.Parse() so they work even
	// when placed after positional arguments.
	checkMode := false
	jsonMode := false
	becomeMode := false
	diffFlag := false
	askBecomePass := false
	var tags, skipTags []string
	limitStr := ""
	serialStr := ""
	vaultPassFileFlag := ""
	askVaultPass := false
	savePlan := ""
	factsTTLStr := ""
	flushCache := false
	askConfirm := false
	modulesPathFlag := "modules"
	modulesPathExplicit := false
	workspaceFlag := ""
	filtered := os.Args[:1]
	for _, a := range os.Args[1:] {
		name, val, hasVal := splitFlag(a)
		switch name {
		case "check":
			checkMode = true
		case "json":
			jsonMode = true
		case "become":
			becomeMode = true
		case "diff":
			diffFlag = true
		case "ask-become-pass":
			askBecomePass = true
		case "ask-vault-pass":
			askVaultPass = true
		case "tags":
			if hasVal {
				tags = append(tags, splitComma(val)...)
			}
		case "skip-tags":
			if hasVal {
				skipTags = append(skipTags, splitComma(val)...)
			}
		case "limit":
			if hasVal {
				limitStr = val
			}
		case "serial":
			if hasVal {
				serialStr = val
			}
		case "vault-pass-file":
			if hasVal {
				vaultPassFileFlag = val
			}
		case "save-plan":
			if hasVal {
				savePlan = val
			}
		case "facts-ttl":
			if hasVal {
				factsTTLStr = val
			}
		case "flush-cache":
			flushCache = true
		case "confirm":
			askConfirm = true
		case "roles-path":
			if hasVal {
				rolesPath = val
			}
		case "modules-path":
			if hasVal {
				modulesPathFlag = val
				modulesPathExplicit = true
			}
		case "workspace":
			if hasVal {
				workspaceFlag = val
			}
		case "workspaces-dir":
			if hasVal {
				workspacesDir = val
			}
		default:
			filtered = append(filtered, a)
		}
	}
	os.Args = filtered

	if jsonMode {
		activeEmitter = newJSONEmitter()
	}
	diffMode = diffFlag
	vaultPassFile = vaultPassFileFlag
	vaultAskPass = askVaultPass

	invPath := flag.String("i", "inventory.yml", "path to inventory file")
	target := flag.String("t", "", "target group or host (e.g. web or 10.0.0.1:2222)")
	parallel := flag.Int("p", 10, "max number of parallel connections")
	reportPath := flag.String("report", "", "write run report to this path (.json or .html)")
	flag.Parse()

	if askBecomePass && os.Getenv("GOAF_BECOME_PASSWORD") == "" {
		pw, err := readPassword("BECOME password: ")
		if err != nil || pw == "" {
			fmt.Fprintln(os.Stderr, "no become password given")
			os.Exit(1)
		}
		becomePassword = pw
	} else if pw := os.Getenv("GOAF_BECOME_PASSWORD"); pw != "" {
		becomePassword = pw
	}

	started := time.Now()

	if err := RegisterExternalModules(modulesPathFlag, modulesPathExplicit); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Workspace selection applies to run/validate/apply (each loads it).
	if err := setupWorkspace(workspaceFlag); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

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

	// vault helper: goaf vault encrypt|decrypt [value|-] (stdin when omitted)
	if args[0] == "vault" {
		runVault(args[1:], vaultPassFileFlag, askVaultPass)
		return
	}

	// modules: list registered (builtin + external) modules
	if args[0] == "modules" {
		for _, n := range listModuleNames() {
			fmt.Println(n)
		}
		return
	}

	// drift: re-check recorded state against live hosts (--fix re-applies)
	if args[0] == "drift" {
		fix := false
		for _, a := range args[1:] {
			if a == "--fix" {
				fix = true
			}
		}
		os.Exit(runDrift(*invPath, becomeMode, *parallel, fix))
	}

	// state: list or forget recorded entries
	if args[0] == "state" {
		if len(args) < 2 || (args[1] != "list" && args[1] != "forget") {
			fmt.Fprintln(os.Stderr, "usage: goaf state list [host] | goaf state forget <host|fingerprint-prefix>")
			os.Exit(1)
		}
		entries := loadState()
		if args[1] == "list" {
			filter := ""
			if len(args) > 2 {
				filter = args[2]
			}
			n := 0
			for _, e := range entries {
				if filter != "" && e.Host != filter {
					continue
				}
				fmt.Printf("%s  %-24s %-12s %-8s %s  %s\n",
					e.Time, e.Host, e.Module, e.Fingerprint, e.Play, describeEntry(e))
				n++
			}
			if n == 0 {
				if filter != "" {
					fmt.Printf("no recorded state for host %q\n", filter)
				} else {
					fmt.Println("no recorded state yet — run something first")
				}
			}
			return
		}
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: goaf state forget <host|fingerprint-prefix>")
			os.Exit(1)
		}
		if n := forgetState(args[2]); n > 0 {
			fmt.Printf("forgot %d %s\n", n, map[bool]string{true: "entry", false: "entries"}[n == 1])
		} else {
			fmt.Printf("nothing matched %q\n", args[2])
		}
		return
	}

	// audit: list recorded runs (newest last)
	if args[0] == "audit" {
		n := 20
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil && v > 0 {
				n = v
			}
		}
		records := listAudit(n)
		if len(records) == 0 {
			fmt.Println("no recorded runs yet (~/.goaf/audit.log)")
			return
		}
		fmt.Printf("%-16s %-8s %-10s %5s %4s %8s %7s  %s\n", "TIME", "USER", "MODE", "HOSTS", "OK", "CHANGED", "FAILED", "DETAIL")
		for _, r := range records {
			detail := r.Target
			if r.Playbook != "" {
				detail = r.Playbook
			}
			fmt.Printf("%-16s %-8s %-10s %5d %4d %8d %7d  %s\n",
				strings.Replace(r.Time, "T", " ", 1)[:16], r.User, r.Mode,
				r.Hosts, r.Ok, r.Changed, r.Failed, detail)
		}
		return
	}

	// apply: run a saved plan file (playbook + inventory + options snapshot)
	if args[0] == "apply" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: goaf apply <plan.json> [--check]")
			os.Exit(1)
		}
		if code := runApply(args[1], *parallel, checkMode, *reportPath, askConfirm); code != 0 {
			os.Exit(code)
		}
		return
	}

	// validate: parse playbook + inventory without connecting anywhere
	if args[0] == "validate" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: goaf -i inventory.yml validate <playbook.yml>")
			os.Exit(1)
		}
		if code := runValidate(args[1], *invPath); code != 0 {
			os.Exit(code)
		}
		return
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
		serialN, serialPct := parseSerial(serialStr)
		opts := RunOptions{
			Parallelism: *parallel,
			CheckMode:   checkMode,
			Become:      becomeMode,
			Tags:        tags,
			SkipTags:    skipTags,
			Limit:       limitStr,
			Serial:      serialN,
			SerialPct:   serialPct,
			FactsTTL:    parseFactsTTL(factsTTLStr),
			FlushCache:  flushCache,
		}
		if askConfirm && !confirmRun(plays, inv, limitStr) {
			fmt.Fprintln(os.Stderr, "aborted by user")
			os.Exit(3)
		}
		if savePlan != "" {
			if err := savePlanFile(savePlan, args[1], *invPath, opts, becomeMode); err != nil {
				fmt.Fprintf(os.Stderr, "error saving plan: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "plan saved to %s\n", savePlan)
		}
		activeEmitter.RunStarted("playbook", 0, *parallel, checkMode)
		failed, report := RunPlaybookOpts(plays, inv, opts)
		if *reportPath != "" {
			if err := writeReport(report, *reportPath); err != nil {
				fmt.Fprintf(os.Stderr, "report error: %v\n", err)
			}
		}
		appendAudit(auditRecord{
			Mode: "playbook", Playbook: args[1], Hosts: len(report.Hosts),
			Ok: report.Summary.Ok, Changed: report.Summary.Changed,
			Failed: report.Summary.Failed, CheckMode: checkMode,
			Ms: time.Since(started).Milliseconds(),
		})
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
			if r.Diff != "" {
				fmt.Printf("    ---\n%s\n", indentDiff(r.Diff))
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

	// Record applied ad-hoc changes for drift detection (idempotent
	// modules only — always-run modules would flag drift forever).
	switch args[0] {
	case "debug", "set_fact", "meta", "command", "upgrade", "reboot", "script", "setup":
	default:
		if !checkMode {
			params := parseCLIParams(args)
			byLabel := make(map[string]Host, len(hosts))
			for _, h := range hosts {
				byLabel[hostLabel(h)] = h
			}
			for _, r := range results {
				if r.Changed && r.Err == nil {
					if h, ok := byLabel[r.Host]; ok {
						recordState(h, "ad-hoc", args[0], params, r.Output)
					}
				}
			}
		}
	}
	appendAudit(auditRecord{
		Mode: "adhoc", Target: *target, Hosts: total,
		Ok: passed + changed, Changed: changed, Failed: failed,
		CheckMode: checkMode, Ms: time.Since(started).Milliseconds(),
	})

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

// knownCLIParams lists the parameter names each module accepts on the CLI.
// An argument is treated as key=value only when the key is a valid identifier
// AND a known parameter of the invoked module (template accepts any key as a
// template variable). Anything else — e.g. shell code like "grep FOO=bar" —
// is positional, so "=" inside commands no longer breaks parsing.
var knownCLIParams = map[string]map[string]bool{
	"command":        {"cmd": true},
	"package":        {"name": true},
	"install":        {"name": true},
	"remove":         {"name": true},
	"copy":           {"src": true, "dest": true, "backup": true},
	"file":           {"path": true, "state": true, "mode": true, "owner": true, "group": true},
	"service":        {"name": true, "state": true, "enabled": true},
	"user":           {"name": true, "state": true, "shell": true, "groups": true},
	"lineinfile":     {"path": true, "line": true, "regexp": true, "state": true},
	"authorized_key": {"user": true, "key": true, "state": true},
	"reboot":         {"timeout": true, "msg": true},
	"debug":          {"var": true, "msg": true},
	"script":         {"src": true, "args": true},
	"fetch":          {"src": true, "dest": true},
}

// isKVArg reports whether s is a key=value pair valid for the given action.
func isKVArg(action, s string) bool {
	idx := strings.Index(s, "=")
	if idx <= 0 {
		return false
	}
	key := s[:idx]
	for i, r := range key {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	if action == "template" || action == "set_fact" {
		return true // any key=value is a variable
	}
	if _, ok := knownCLIParams[action]; !ok {
		// External modules accept any key=value pair.
		if externalModuleNames[action] {
			for i, r := range key {
				if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
					continue
				}
				return false
			}
			return true
		}
		return false
	}
	allowed, ok := knownCLIParams[action]
	return ok && allowed[key]
}

// parseCLIParams parses arguments as key=value pairs, with positional fallback
// for command and install/remove modules.
// Args starting with "-" are silently skipped (misplaced flags).
// KV args are always parsed even if non-KV args are present.
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
		if isKVArg(action, a) {
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

// parseSerial parses --serial=N (hosts per batch) or --serial=N% (percent).
// Returns (count, percent); only one is non-zero.
func parseSerial(s string) (int, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0
	}
	if strings.HasSuffix(s, "%") {
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(s, "%")))
		if err != nil || n <= 0 || n > 100 {
			fmt.Fprintf(os.Stderr, "invalid --serial value %q (want 1-100%%)\n", s)
			os.Exit(1)
		}
		return 0, n
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "invalid --serial value %q (want a non-negative integer)\n", s)
		os.Exit(1)
	}
	return n, 0
}

// confirmRun asks "run N play(s) on M host(s)? [y/N]" on a terminal.
// Without a terminal (piped/CI) it prints a notice and proceeds.
func confirmRun(plays []Play, inv *Inventory, limit string) bool {
	set := map[string]bool{}
	for _, play := range plays {
		hosts, err := inv.Resolve(play.Hosts)
		if err != nil {
			continue
		}
		for _, h := range hosts {
			set[hostLabel(h)] = true
		}
	}
	if limit != "" {
		limHosts, err := inv.Resolve(limit)
		if err == nil {
			lim := map[string]bool{}
			for _, h := range limHosts {
				lim[hostLabel(h)] = true
			}
			for label := range set {
				if !lim[label] {
					delete(set, label)
				}
			}
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "--confirm: no terminal, proceeding with %d play(s) on %d host(s)\n", len(plays), len(set))
		return true
	}
	fmt.Fprintf(os.Stderr, "Run %d play(s) on %d host(s)? [y/N]: ", len(plays), len(set))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// parseFactsTTL parses --facts-ttl=N (seconds, default 3600).
func parseFactsTTL(s string) int {
	if s == "" {
		return 3600
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "invalid --facts-ttl value %q (want seconds >= 0, 0 disables cache)\n", s)
		os.Exit(1)
	}
	return n
}

// runPlan is a saved playbook run: playbook + inventory contents and the
// run options, so `apply` reproduces the exact same run later.
type runPlan struct {
	Version       int      `json:"version"`
	Created       string   `json:"created"`
	PlaybookName  string   `json:"playbook"`
	PlaybookB64   string   `json:"playbook_b64"`
	InventoryName string   `json:"inventory"`
	InventoryB64  string   `json:"inventory_b64"`
	Parallelism   int      `json:"parallelism"`
	Become        bool     `json:"become"`
	Tags          []string `json:"tags"`
	SkipTags      []string `json:"skip_tags"`
	Limit         string   `json:"limit"`
	Serial        int      `json:"serial"`
	SerialPct     int      `json:"serial_pct"`
}

// savePlanFile snapshots the playbook/inventory files and run options.
func savePlanFile(path, playbookPath, invPath string, opts RunOptions, become bool) error {
	pbData, err := os.ReadFile(playbookPath)
	if err != nil {
		return err
	}
	invData, err := os.ReadFile(invPath)
	if err != nil {
		return err
	}
	plan := runPlan{
		Version:       1,
		Created:       time.Now().Format(time.RFC3339),
		PlaybookName:  filepath.Base(playbookPath),
		PlaybookB64:   base64.StdEncoding.EncodeToString(pbData),
		InventoryName: filepath.Base(invPath),
		InventoryB64:  base64.StdEncoding.EncodeToString(invData),
		Parallelism:   opts.Parallelism,
		Become:        become,
		Tags:          opts.Tags,
		SkipTags:      opts.SkipTags,
		Limit:         opts.Limit,
		Serial:        opts.Serial,
		SerialPct:     opts.SerialPct,
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// runApply executes a saved plan file. --check forces dry-run on top.
func runApply(planPath string, parallel int, forceCheck bool, reportPath string, askConfirm bool) int {
	started := time.Now()
	_ = parallel
	raw, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading plan: %v\n", err)
		return 1
	}
	var plan runPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing plan: %v\n", err)
		return 1
	}
	if plan.Version != 1 {
		fmt.Fprintf(os.Stderr, "unsupported plan version %d\n", plan.Version)
		return 1
	}
	pbData, err := base64.StdEncoding.DecodeString(plan.PlaybookB64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error decoding plan playbook: %v\n", err)
		return 1
	}
	invData, err := base64.StdEncoding.DecodeString(plan.InventoryB64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error decoding plan inventory: %v\n", err)
		return 1
	}
	tmpDir, err := os.MkdirTemp("", "goaf-plan-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)
	pbPath := filepath.Join(tmpDir, plan.PlaybookName)
	invPath := filepath.Join(tmpDir, plan.InventoryName)
	if err := os.WriteFile(pbPath, pbData, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(invPath, invData, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	inv, err := LoadInventory(invPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading plan inventory: %v\n", err)
		return 1
	}
	plays, err := loadPlaybook(pbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading plan playbook: %v\n", err)
		return 1
	}
	if askConfirm && !confirmRun(plays, inv, plan.Limit) {
		fmt.Fprintln(os.Stderr, "aborted by user")
		return 3
	}
	opts := RunOptions{
		Parallelism: plan.Parallelism,
		CheckMode:   forceCheck,
		Become:      plan.Become,
		Tags:        plan.Tags,
		SkipTags:    plan.SkipTags,
		Limit:       plan.Limit,
		Serial:      plan.Serial,
		SerialPct:   plan.SerialPct,
		FactsTTL:    3600,
	}
	activeEmitter.RunStarted("playbook", 0, opts.Parallelism, opts.CheckMode)
	failed, report := RunPlaybookOpts(plays, inv, opts)
	if reportPath != "" {
		if err := writeReport(report, reportPath); err != nil {
			fmt.Fprintf(os.Stderr, "error writing report: %v\n", err)
		}
	}
	appendAudit(auditRecord{
		Mode: "apply", Playbook: plan.PlaybookName + " < " + planPath,
		Hosts: len(report.Hosts), Ok: report.Summary.Ok,
		Changed: report.Summary.Changed, Failed: report.Summary.Failed,
		CheckMode: opts.CheckMode, Ms: time.Since(started).Milliseconds(),
	})
	if failed > 0 {
		return 2
	}
	return 0
}

// runVault implements `goaf vault encrypt|decrypt [value]`.
// Without a value the data is read from stdin.
func runVault(args []string, passFile string, askPass bool) {
	if len(args) < 1 || (args[0] != "encrypt" && args[0] != "decrypt") {
		fmt.Fprintln(os.Stderr, "usage: goaf vault encrypt|decrypt [value|-]\n  password via --vault-pass-file, --ask-vault-pass or GOAF_VAULT_PASSWORD")
		os.Exit(1)
	}
	pw, err := vaultPassword(passFile, askPass)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault: %v\n", err)
		os.Exit(1)
	}
	var input string
	if len(args) >= 2 && args[1] != "-" {
		input = args[1]
	} else {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vault: reading stdin: %v\n", err)
			os.Exit(1)
		}
		input = strings.TrimRight(string(data), "\n")
	}
	if args[0] == "encrypt" {
		out, err := encryptVault(input, pw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vault: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(out)
		return
	}
	out, err := decryptVault(strings.TrimSpace(input), pw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// runValidate parses a playbook and inventory without connecting anywhere.
// Returns process exit code (0 = valid).
func runValidate(playPath, invPath string) int {
	plays, err := loadPlaybook(playPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "playbook %q: %v\n", playPath, err)
		return 2
	}
	inv, err := LoadInventory(invPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inventory %q: %v\n", invPath, err)
		return 2
	}
	failed := 0
	for _, play := range plays {
		hosts, err := inv.Resolve(play.Hosts)
		if err != nil {
			fmt.Printf("PLAY [%s]: ERROR resolving hosts %q: %v\n", play.Name, play.Hosts, err)
			failed++
			continue
		}
		handlers := map[string]bool{}
		for _, h := range play.Handlers {
			handlers[h.Name] = true
		}
		// Dummy context for validation: loop items, gathered facts,
		// registered and set_fact names only exist at runtime — provide
		// placeholders so only genuinely broken templates fail validation.
		dummyVars := map[string]string{"item": "ITEM"}
		for _, f := range []string{"goaf_hostname", "goaf_arch", "goaf_kernel", "goaf_ip", "goaf_os", "goaf_os_name", "goaf_os_version", "goaf_os_family"} {
			dummyVars[f] = "FACT"
		}
		var collectRuntimeVars func(tasks []PlayTask)
		collectRuntimeVars = func(tasks []PlayTask) {
			for _, t := range tasks {
				if t.Register != "" {
					dummyVars[t.Register] = "REGISTERED"
				}
				if t.Module == "set_fact" {
					for k := range t.Params {
						dummyVars[k] = "FACT"
					}
				}
				if t.Module == "block" {
					collectRuntimeVars(t.Block)
					collectRuntimeVars(t.Rescue)
					collectRuntimeVars(t.Always)
				}
			}
		}
		collectRuntimeVars(play.PreTasks)
		collectRuntimeVars(play.Tasks)
		collectRuntimeVars(play.PostTasks)
		checkVars := mergeVars(mergeVars(workspaceVars, play.Vars), dummyVars)
		playFailed := 0
		for _, section := range [][]PlayTask{play.PreTasks, play.Tasks, play.PostTasks} {
			playFailed += validateTaskList(play.Name, section, checkVars, handlers)
		}
		if playFailed == 0 {
			fmt.Printf("PLAY [%s]: OK (%d hosts, %d tasks)\n", play.Name, len(hosts), len(play.Tasks))
		} else {
			failed += playFailed
		}
	}
	if failed > 0 {
		fmt.Printf("INVALID: %d problem(s)\n", failed)
		return 2
	}
	fmt.Println("VALID")
	return 0
}

// validateTaskList checks one task list (recursing into blocks).
func validateTaskList(playName string, tasks []PlayTask, checkVars map[string]string, handlers map[string]bool) int {
	failed := 0
	for _, t := range tasks {
		if t.Module == "block" {
			if len(t.Block) == 0 {
				fmt.Printf("PLAY [%s] task %q: block is empty\n", playName, t.Name)
				failed++
				continue
			}
			for _, sub := range [][]PlayTask{t.Block, t.Rescue, t.Always} {
				failed += validateTaskList(playName, sub, checkVars, handlers)
			}
			continue
		}
		factory, ok := LookupModule(t.Module)
		if !ok {
			fmt.Printf("PLAY [%s] task %q: unknown module %q\n", playName, t.Name, t.Module)
			failed++
			continue
		}
		if _, err := expandVars(t.Params, checkVars); err != nil {
			fmt.Printf("PLAY [%s] task %q: %v\n", playName, t.Name, err)
			failed++
			continue
		}
		for _, expr := range []string{t.When, t.FailedWhen, t.ChangedWhen, t.Until} {
			if expr != "" {
				if _, err := evalWhen(expr, checkVars); err != nil {
					fmt.Printf("PLAY [%s] task %q: bad condition: %v\n", playName, t.Name, err)
					failed++
					break
				}
			}
		}
		_ = factory
		if t.Notify != "" && !handlers[t.Notify] {
			fmt.Printf("PLAY [%s] task %q: notify target %q has no handler\n", playName, t.Name, t.Notify)
			failed++
		}
	}
	return failed
}

func usage() {
	fmt.Println("Usage:")
	fmt.Println("  goaf [-check] [-json] [-diff] -i inventory.yml -t <group|host> <module> [params]")
	fmt.Println("  goaf [-check] [-json] [-diff] -i inventory.yml run <playbook.yml>")
	fmt.Println("  goaf -i inventory.yml validate <playbook.yml>")
	fmt.Println("  goaf -i inventory.yml run <playbook.yml> --save-plan=<plan.json>")
	fmt.Println("  goaf apply <plan.json> [--check]")
	fmt.Println("  goaf vault encrypt|decrypt [value|-]")
	fmt.Println("  goaf -i inventory.yml drift [--fix]")
	fmt.Println("  goaf state list [host]")
	fmt.Println("  goaf state forget <host|fingerprint-prefix>")
	fmt.Println("  goaf audit [N]")
	fmt.Println("\nFlags:")
	fmt.Println("  -i <path>       inventory file (default: inventory.yml);")
	fmt.Println("                  exec:<command> or an executable script = dynamic inventory")
	fmt.Println("  -t <target>     group name or host:port for ad-hoc")
	fmt.Println("  -p <n>          parallelism (default: 10)")
	fmt.Println("  -check          dry-run — show what would change, skip Apply()")
	fmt.Println("  -become         run tasks with sudo (privilege escalation)")
	fmt.Println("  -ask-become-pass  prompt for the sudo password (or GOAF_BECOME_PASSWORD)")
	fmt.Println("  -diff           show unified diffs for copy/template/file/lineinfile changes")
	fmt.Println("  -json           emit NDJSON event stream (one JSON object per line)")
	fmt.Println("  -report <path>  write run report (.json or .html)")
	fmt.Println("  --tags=a,b      run only tasks with these tags (playbook mode)")
	fmt.Println("  --skip-tags=a   skip tasks with these tags (playbook mode)")
	fmt.Println("  --limit=<expr>  restrict playbook run to matching hosts")
	fmt.Println("  --serial=<n>    max hosts per batch — rolling update (playbook mode)")
	fmt.Println("  --vault-pass-file=<path>  password for $GOAFVAULT values (or GOAF_VAULT_PASSWORD)")
	fmt.Println("  --ask-vault-pass          prompt for the vault password")
	fmt.Println("  --facts-ttl=<sec>  reuse cached facts this fresh (default 3600, 0 disables)")
	fmt.Println("  --flush-cache      ignore cached facts and refresh them")
	fmt.Println("  --confirm          ask [y/N] before applying a playbook run")
	fmt.Println("  --modules-path=<dir>  external modules dir (default ./modules if present)")
	fmt.Println("  --roles-path=<dir>    roles base dir (default ./roles if present)")
	fmt.Println("  --workspace=<name>    environment vars from workspaces/<name>.yml")
	fmt.Println("  --workspaces-dir=<dir>  workspaces base dir (default ./workspaces)")
	fmt.Println("\nModules (ad-hoc):")
	fmt.Println("  command  \"<shell command>\"")
	fmt.Println("  install  <package>")
	fmt.Println("  remove   <package>")
	fmt.Println("  upgrade  (upgrade all packages)")
	fmt.Println("  copy     src=<local> dest=<remote> [backup=true]")
	fmt.Println("  file     path=<path> [state=file|directory|absent] [mode=0644] [owner=root] [group=root]")
	fmt.Println("  lineinfile path=<path> line=<line> [regexp=<re>] [state=present|absent]")
	fmt.Println("  service  name=<service> [state=started|stopped|restarted] [enabled=true|false]")
	fmt.Println("  template src=<template> dest=<remote> [backup=true] [key=value ...]")
	fmt.Println("  user     name=<user> [state=present|absent] [shell=<sh>] [groups=a,b]")
	fmt.Println("  authorized_key user=<user> key=\"<pubkey>\" [state=present|absent]")
	fmt.Println("  reboot   [timeout=300] (reboot and wait for SSH)")
	fmt.Println("  script   src=<script> [args=<args>] (upload and execute, Linux-only)")
	fmt.Println("  fetch    src=<remote> dest=<local> (download file)")
	fmt.Println("  debug    var=<name> | msg=\"<text>\" (playbook: show value)")
	fmt.Println("  set_fact <key=value ...> (playbook: define host variables)")
	fmt.Println("  setup    (display gathered host facts: os, hostname, arch, kernel, ip)")
	fmt.Println("\nPlaybook (run):")
	fmt.Println("  goaf -i inventory.yml run site.yml")
	fmt.Println("  goaf -check -i inventory.yml run site.yml   # dry-run")
	fmt.Println("  goaf -json  -i inventory.yml run site.yml   # NDJSON output")
	fmt.Println("\nPlaybook task keys: when, failed_when, changed_when, ignore_errors,")
	fmt.Println("  register, loop/with_items, notify, tags, run_once, delegate_to,")
	fmt.Println("  block/rescue/always, retries/delay/until, meta (+ pre_tasks/post_tasks,")
	fmt.Println("  max_fail_percentage/any_errors_fatal, top-level handlers:)")
	fmt.Println("\nExamples:")
	fmt.Println("  goaf -t web command \"uptime\"")
	fmt.Println("  goaf -t web install nginx")
	fmt.Println("  goaf -t web copy src=./nginx.conf dest=/etc/nginx/nginx.conf")
	fmt.Println("  goaf -t web file path=/var/www state=directory mode=0755")
	fmt.Println("  goaf -t web service name=nginx state=started enabled=true")
	fmt.Println("  goaf -t web template src=./nginx.conf.tmpl dest=/etc/nginx/nginx.conf port=80")
}
