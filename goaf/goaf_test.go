package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiffBasic(t *testing.T) {
	old := "line1\nline2-old\nline3\n"
	new := "line1\nline2-NEW\nline3\nline4\n"
	d := unifiedDiff("f", "f", old, new, 3)
	if d == "" {
		t.Fatal("expected non-empty diff")
	}
	for _, want := range []string{"--- f", "+++ f", "@@", "-line2-old", "+line2-NEW", "+line4"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	d := unifiedDiff("f", "f", "a\nb\n", "a\nb\n", 3)
	if d != "" {
		t.Errorf("expected empty diff, got:\n%s", d)
	}
}

func TestUnifiedDiffEmptyOld(t *testing.T) {
	d := unifiedDiff("f", "f", "", "a\n", 3)
	if !strings.Contains(d, "+a") {
		t.Errorf("expected +a in diff, got:\n%s", d)
	}
}

func TestShQuote(t *testing.T) {
	if got := shQuote("abc"); got != "'abc'" {
		t.Errorf("got %q", got)
	}
	if got := shQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("got %q", got)
	}
}

func TestIsKVArg(t *testing.T) {
	cases := []struct {
		action, arg string
		want        bool
	}{
		{"command", "cmd=uptime", true},
		{"command", "grep FOO=bar /x", false},
		{"command", "FOO=bar", false},
		{"command", "uptime", false},
		{"install", "name=bash", true},
		{"install", "bash", false},
		{"copy", "src=a", true},
		{"copy", "dest=b", true},
		{"copy", "bogus=1", false},
		{"template", "port=8080", true},
		{"template", "anything=1", true},
		{"file", "path=/x", true},
		{"file", "mode=0644", true},
		{"service", "enabled=true", true},
		{"setup", "x=1", false},
	}
	for _, c := range cases {
		if got := isKVArg(c.action, c.arg); got != c.want {
			t.Errorf("isKVArg(%q,%q)=%v want %v", c.action, c.arg, got, c.want)
		}
	}
}

func TestParseCLIParams(t *testing.T) {
	p := parseCLIParams([]string{"command", "grep FOO=bar /etc/os-release"})
	if p["cmd"] != "grep FOO=bar /etc/os-release" {
		t.Errorf("got %v", p)
	}
	p = parseCLIParams([]string{"command", "cmd=echo a=b"})
	if p["cmd"] != "echo a=b" {
		t.Errorf("got %v", p)
	}
	p = parseCLIParams([]string{"install", "bash"})
	if p["name"] != "bash" {
		t.Errorf("got %v", p)
	}
}

func TestEvalWhen(t *testing.T) {
	vars := map[string]string{"env": "prod", "goaf_os_family": "debian"}
	for expr, want := range map[string]bool{
		`{{eq .env "prod"}}`:              true,
		`{{eq .goaf_os_family "debian"}}`: true,
		`{{eq .env "dev"}}`:               false,
		`{{.missing}}`:                    false,
		`true`:                            true,
		`false`:                           false,
	} {
		got, err := evalWhen(expr, vars)
		if err != nil {
			t.Errorf("evalWhen(%q) err: %v", expr, err)
			continue
		}
		if got != want {
			t.Errorf("evalWhen(%q)=%v want %v", expr, got, want)
		}
	}
}

func TestExpandVars(t *testing.T) {
	vars := map[string]string{"pkg": "nginx", "port": "80"}
	got, err := expandVars(map[string]string{"name": "{{.pkg}}", "port": "{{.port}}", "plain": "x"}, vars)
	if err != nil {
		t.Fatal(err)
	}
	if got["name"] != "nginx" || got["port"] != "80" || got["plain"] != "x" {
		t.Errorf("got %v", got)
	}
	if _, err := expandVars(map[string]string{"x": "{{.nope}}"}, vars); err == nil {
		t.Error("expected missingkey error")
	}
}

func TestInventoryResolve(t *testing.T) {
	inv := &Inventory{
		Groups: map[string]Group{
			"web": {Hosts: []string{"h1", "h2"}, Vars: map[string]string{"role": "web"}},
			"db":  {Hosts: []string{"h3"}},
			"all": {Children: []string{"web", "db"}},
		},
		Hosts: map[string]HostEntry{
			"h1": {Vars: map[string]string{"role": "special"}},
		},
	}
	inv.Vars.User = "root"
	hosts, err := inv.Resolve("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts", len(hosts))
	}
	if _, err := inv.Resolve("nope-nosuch"); err != nil {
		t.Logf("direct host ok: %v", err)
	}
	// group/host vars merge: host entry wins over group
	v := inv.varsForHost(Host{Addr: "h1", User: "root", Port: 22})
	if v["role"] != "special" {
		t.Errorf("host var should win, got %v", v)
	}
	v = inv.varsForHost(Host{Addr: "h2", User: "root", Port: 22})
	if v["role"] != "web" {
		t.Errorf("group var missing, got %v", v)
	}
}

func TestInventoryCircular(t *testing.T) {
	inv := &Inventory{
		Groups: map[string]Group{
			"a": {Children: []string{"b"}},
			"b": {Children: []string{"a"}},
		},
	}
	if _, err := inv.Resolve("a"); err == nil {
		t.Error("expected circular reference error")
	}
}

func TestParseTasks(t *testing.T) {
	plays, err := loadPlaybookBytes([]byte(`- name: p
  hosts: all
  tasks:
    - name: t1
      command: uptime
      register: out
      failed_when: '{{eq .result "x"}}'
      ignore_errors: true
      tags: [a, b]
    - name: t2
      file:
        path: /tmp/x
        state: directory
      loop: [1, 2]
      notify: H
  handlers:
    - name: H
      command: echo hi
`), "", "roles")
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 {
		t.Fatalf("got %d plays", len(plays))
	}
	t1 := plays[0].Tasks[0]
	if t1.Module != "command" || t1.Register != "out" || !t1.IgnoreErrs {
		t.Errorf("bad task parse: %+v", t1)
	}
	if len(t1.Tags) != 2 || t1.FailedWhen == "" {
		t.Errorf("bad tags/when: %+v", t1)
	}
	t2 := plays[0].Tasks[1]
	if len(t2.Loop) != 2 || t2.Notify != "H" || t2.Params["path"] != "/tmp/x" {
		t.Errorf("bad task2: %+v", t2)
	}
	if len(plays[0].Handlers) != 1 {
		t.Error("handlers not parsed")
	}
}

func TestParseBlock(t *testing.T) {
	plays, err := loadPlaybookBytes([]byte(`- name: p
  hosts: all
  pre_tasks:
    - name: Pre
      command: uptime
  tasks:
    - name: Deploy
      block:
        - name: Do it
          command: uptime
        - name: Might fail
          command: "exit 1"
      rescue:
        - name: Fix
          command: uptime
      always:
        - name: Clean
          command: uptime
      when: '{{eq .env "prod"}}'
  post_tasks:
    - name: Post
      command: uptime
`), "", "roles")
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 {
		t.Fatalf("got %d plays", len(plays))
	}
	p := plays[0]
	if len(p.PreTasks) != 1 || len(p.PostTasks) != 1 {
		t.Errorf("pre/post not parsed: %+v", p)
	}
	if len(p.Tasks) != 1 {
		t.Fatalf("got %d tasks", len(p.Tasks))
	}
	b := p.Tasks[0]
	if b.Module != "block" || len(b.Block) != 2 || len(b.Rescue) != 1 || len(b.Always) != 1 {
		t.Errorf("bad block parse: %+v", b)
	}
	if b.When == "" {
		t.Error("block when not parsed")
	}
}

func TestParseRunOnceDelegate(t *testing.T) {
	plays, err := loadPlaybookBytes([]byte(`- name: p
  hosts: all
  tasks:
    - name: Once
      command: uptime
      run_once: true
      delegate_to: localhost
      tags: always
`), "", "roles")
	if err != nil {
		t.Fatal(err)
	}
	task := plays[0].Tasks[0]
	if !task.RunOnce || task.DelegateTo != "localhost" {
		t.Errorf("bad parse: %+v", task)
	}
	if len(task.Tags) != 1 || task.Tags[0] != "always" {
		t.Errorf("bad tags: %+v", task.Tags)
	}
}

func TestParseDebugSetFact(t *testing.T) {
	plays, err := loadPlaybookBytes([]byte(`- name: p
  hosts: all
  tasks:
    - name: D1
      debug: var=osline
    - name: D2
      debug: {msg: "hi {{.x}}"}
    - name: S1
      set_fact: {a: "1", b: "2"}
`), "", "roles")
	if err != nil {
		t.Fatal(err)
	}
	tasks := plays[0].Tasks
	if tasks[0].Module != "debug" || tasks[0].Params["var"] != "osline" {
		t.Errorf("bad debug shorthand: %+v", tasks[0])
	}
	if tasks[1].Params["msg"] != "hi {{.x}}" {
		t.Errorf("bad debug map: %+v", tasks[1])
	}
	if tasks[2].Module != "set_fact" || tasks[2].Params["b"] != "2" {
		t.Errorf("bad set_fact: %+v", tasks[2])
	}
}

func TestParseInventoryBytes(t *testing.T) {
	inv, err := parseInventoryBytes([]byte(`groups:
  web:
    hosts: [h1]
    vars: {role: web}
hosts:
  h1: {vars: {role: special}}
vars: {user: root}
`))
	if err != nil {
		t.Fatal(err)
	}
	if v := inv.varsForHost(Host{Addr: "h1", User: "root", Port: 22}); v["role"] != "special" {
		t.Errorf("host var should win: %v", v)
	}
}

func TestIsInventoryFile(t *testing.T) {
	for _, p := range []string{"a.yml", "a.yaml", "a.json", "A.YML"} {
		if !isInventoryFile(p) {
			t.Errorf("%q should be inventory file", p)
		}
	}
	for _, p := range []string{"gen.sh", "gen.bat", "gen.ps1", "noext"} {
		if isInventoryFile(p) {
			t.Errorf("%q should not be inventory file", p)
		}
	}
}

func TestParseRetriesUntilMeta(t *testing.T) {
	plays, err := loadPlaybookBytes([]byte(`- name: p
  hosts: all
  max_fail_percentage: 30
  any_errors_fatal: true
  tasks:
    - name: Retry me
      command: uptime
      retries: 5
      delay: 3
      until: '{{eq .result "ok"}}'
    - name: Flush now
      meta: flush_handlers
`), "", "roles")
	if err != nil {
		t.Fatal(err)
	}
	p := plays[0]
	if p.MaxFailPct == nil || *p.MaxFailPct != 30 || !p.AnyErrorsFatal {
		t.Errorf("bad play abort keys: %+v", p)
	}
	rt := p.Tasks[0]
	if rt.Retries != 5 || rt.Delay != 3 || rt.Until == "" {
		t.Errorf("bad retry parse: %+v", rt)
	}
	if p.Tasks[1].Module != "meta" {
		t.Errorf("bad meta parse: %+v", p.Tasks[1])
	}
}

func TestExpandRoles(t *testing.T) {
	dir := t.TempDir()
	mk := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("roles/web/tasks/main.yml", "- name: Deploy\n  command: uptime\n")
	mk("roles/web/handlers/main.yml", "- name: Restart\n  command: uptime\n")
	mk("roles/web/vars/main.yml", "port: \"9090\"\n")
	mk("roles/web/defaults/main.yml", "port: \"80\"\nfb: dflt\n")
	mk("roles/web/files/app.txt", "x")
	mk("site.yml", "- name: p\n  hosts: all\n  vars: {port: \"7070\"}\n  roles:\n    - web\n")

	data, err := os.ReadFile(filepath.Join(dir, "site.yml"))
	if err != nil {
		t.Fatal(err)
	}
	plays, err := loadPlaybookBytes(data, dir, filepath.Join(dir, "roles"))
	if err != nil {
		t.Fatal(err)
	}
	p := plays[0]
	if len(p.Tasks) != 1 || len(p.Handlers) != 1 {
		t.Fatalf("bad role expansion: %+v", p)
	}
	if !strings.HasPrefix(p.Tasks[0].Name, "web : ") {
		t.Errorf("role prefix missing: %q", p.Tasks[0].Name)
	}
	if p.Tasks[0].RoleDir == "" {
		t.Error("RoleDir not set")
	}
	if p.Vars["port"] != "9090" || p.Vars["fb"] != "dflt" {
		t.Errorf("bad var merge: %v", p.Vars)
	}
}

func TestIncludeTasks(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "common.yml")
	if err := os.WriteFile(inc, []byte("- name: Inc\n  command: uptime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plays, err := loadPlaybookBytes([]byte("- name: p\n  hosts: all\n  tasks:\n    - name: Wrapper\n      include_tasks: common.yml\n"), dir, "roles")
	if err != nil {
		t.Fatal(err)
	}
	if len(plays[0].Tasks) != 1 || plays[0].Tasks[0].Module != "block" {
		t.Fatalf("include not spliced as block: %+v", plays[0].Tasks)
	}
	if len(plays[0].Tasks[0].Block) != 1 || plays[0].Tasks[0].Block[0].Name != "Inc" {
		t.Errorf("bad include content: %+v", plays[0].Tasks[0])
	}
}

func TestParseSerial(t *testing.T) {
	n, pct := parseSerial("")
	if n != 0 || pct != 0 {
		t.Errorf("empty: got %d/%d", n, pct)
	}
	n, pct = parseSerial("2")
	if n != 2 || pct != 0 {
		t.Errorf("int: got %d/%d", n, pct)
	}
	n, pct = parseSerial("40%")
	if n != 0 || pct != 40 {
		t.Errorf("pct: got %d/%d", n, pct)
	}
}

func TestEffectiveSerial(t *testing.T) {
	if got := effectiveSerial(RunOptions{SerialPct: 40}, 5); got != 2 {
		t.Errorf("40%% of 5: got %d", got)
	}
	if got := effectiveSerial(RunOptions{SerialPct: 50}, 1); got != 1 {
		t.Errorf("min 1: got %d", got)
	}
	if got := effectiveSerial(RunOptions{Serial: 3}, 10); got != 3 {
		t.Errorf("int: got %d", got)
	}
	if got := effectiveSerial(RunOptions{}, 10); got != 0 {
		t.Errorf("unset: got %d", got)
	}
}

func TestConfirmNonTerminal(t *testing.T) {
	// go test stdin is not a terminal → proceeds with notice
	inv := &Inventory{Groups: map[string]Group{
		"web": {Hosts: []string{"h1", "h2"}},
	}}
	plays := []Play{{Name: "p", Hosts: "web"}}
	if !confirmRun(plays, inv, "") {
		t.Error("non-terminal should proceed")
	}
}

func TestPlayOutputs(t *testing.T) {
	report := newReport("playbook", false)
	play := Play{Name: "p", Vars: map[string]string{"env": "prod"},
		Outputs: map[string]string{"salute": "hi-{{.env}}", "bad": "{{.nope}}\""}}
	inv := &Inventory{}
	facts := map[string]Facts{"h1": {"goaf_os": "debian"}}
	reg := map[string]map[string]string{}
	printPlayOutputs(play, inv, Host{Addr: "h1"}, facts, reg, report)
	if report.Outputs["salute"] != "hi-prod" {
		t.Errorf("bad outputs: %v", report.Outputs)
	}
	if _, ok := report.Outputs["bad"]; ok {
		t.Errorf("broken output should be skipped: %v", report.Outputs)
	}
}

func TestParseExtResult(t *testing.T) {
	r, err := parseExtResult("needed: true\noutput: hello\n", "needed")
	if err != nil || !r.flag || r.output != "hello" {
		t.Errorf("got %+v, %v", r, err)
	}
	r, err = parseExtResult("\n  changed: false  \n", "changed")
	if err != nil || r.flag {
		t.Errorf("got %+v, %v", r, err)
	}
	r, err = parseExtResult("error: boom\n", "needed")
	if err != nil || r.errMsg != "boom" {
		t.Errorf("got %+v, %v", r, err)
	}
	if _, err := parseExtResult("garbage\n", "needed"); err == nil {
		t.Error("expected error for garbage")
	}
	if _, err := parseExtResult("changed: true\n", "needed"); err == nil {
		t.Error("expected error for wrong key")
	}
}

func TestRegisterExternalModules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mymod.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := listModuleNames()
	for _, n := range names {
		if n == "mymod" {
			t.Fatal("mymod registered too early")
		}
	}
	if err := RegisterExternalModules(dir, true); err != nil {
		t.Fatal(err)
	}
	if !externalModuleNames["mymod"] {
		t.Error("mymod not tracked")
	}
	if _, ok := LookupModule("mymod"); !ok {
		t.Error("mymod factory missing")
	}
	if err := RegisterExternalModules(filepath.Join(dir, "nope"), true); err == nil {
		t.Error("expected error for explicit missing dir")
	}
	if err := RegisterExternalModules(filepath.Join(dir, "nope"), false); err != nil {
		t.Errorf("default missing dir should be ignored: %v", err)
	}
}

func TestSetupWorkspace(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "vars:\n  db: db-dev\nflat: plain\n"
	if err := os.WriteFile(filepath.Join(wsDir, "dev.yml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, oldVars, oldName := workspacesDir, workspaceVars, workspaceName
	workspacesDir = wsDir
	defer func() { workspacesDir, workspaceVars, workspaceName = oldDir, oldVars, oldName }()
	if err := setupWorkspace("dev"); err != nil {
		t.Fatal(err)
	}
	if workspaceName != "dev" || workspaceVars["db"] != "db-dev" || workspaceVars["workspace"] != "dev" {
		t.Errorf("bad workspace: %q %v", workspaceName, workspaceVars)
	}
	if err := setupWorkspace("nope"); err == nil {
		t.Error("expected error for missing workspace")
	}
	// flat form
	if err := os.WriteFile(filepath.Join(wsDir, "flat.yml"), []byte("a: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setupWorkspace("flat"); err != nil {
		t.Fatal(err)
	}
	if workspaceVars["a"] != "1" {
		t.Errorf("flat form failed: %v", workspaceVars)
	}
}

func TestFingerprintStable(t *testing.T) {
	a := fingerprintOf("h1", "copy", map[string]string{"b": "2", "a": "1"})
	b := fingerprintOf("h1", "copy", map[string]string{"a": "1", "b": "2"})
	if a != b || a == "" {
		t.Errorf("fingerprint must be order-independent: %q vs %q", a, b)
	}
	if c := fingerprintOf("h1", "copy", map[string]string{"a": "1"}); c == a {
		t.Error("different params must differ")
	}
}

func TestForgetState(t *testing.T) {
	dir := t.TempDir()
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	defer func() {
		if oldHome != "" {
			t.Setenv("HOME", oldHome)
		}
	}()
	h := Host{Addr: "h1", User: "root", Port: 22}
	recordState(h, "p", "copy", map[string]string{"dest": "/x"}, "out")
	recordState(h, "p", "file", map[string]string{"path": "/y"}, "out")
	if len(loadState()) != 2 {
		t.Fatalf("got %d entries", len(loadState()))
	}
	if n := forgetState("h1"); n != 2 {
		t.Errorf("forget host: removed %d", n)
	}
	if len(loadState()) != 0 {
		t.Error("state should be empty")
	}
	recordState(h, "p", "copy", map[string]string{"dest": "/x"}, "out")
	entries := loadState()
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if n := forgetState(entries[0].Fingerprint[:8]); n != 1 {
		t.Errorf("forget by prefix: removed %d", n)
	}
	if n := forgetState("nope"); n != 0 {
		t.Errorf("forget missing: removed %d", n)
	}
}

func TestPsQuote(t *testing.T) {
	if got := psQuote("abc"); got != "'abc'" {
		t.Errorf("got %q", got)
	}
	if got := psQuote("a'b"); got != "'a''b'" {
		t.Errorf("got %q", got)
	}
}

func TestWinParent(t *testing.T) {
	cases := map[string]string{
		`C:/Temp/x.txt`:  `C:/Temp`,
		`C:\Temp\x.txt`:  `C:\Temp`,
		`C:/x.txt`:       `C:/`,
		`relative/f.txt`: `relative`,
		`plain.txt`:      "",
	}
	for in, want := range cases {
		if got := winParent(in); got != want {
			t.Errorf("winParent(%q)=%q want %q", in, got, want)
		}
	}
}

func TestInventoryWinRM(t *testing.T) {
	inv, err := parseInventoryBytes([]byte(`groups:
  win:
    hosts: [10.0.0.5]
hosts:
  10.0.0.5:
    user: admin
    connection: winrm
    password: s3cret
vars: {user: root}
`))
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := inv.Resolve("win")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts", len(hosts))
	}
	h := hosts[0]
	if h.Connection != "winrm" || h.Password != "s3cret" || h.User != "admin" {
		t.Errorf("bad winrm host: %+v", h)
	}
	if h.Port != 5985 {
		t.Errorf("default winrm port should be 5985, got %d", h.Port)
	}
}

func TestTagsAllow(t *testing.T) {
	task := PlayTask{Name: "t", Tags: []string{"demo"}}
	if !tagsAllow(task, RunOptions{}) {
		t.Error("no filter should allow")
	}
	if !tagsAllow(task, RunOptions{Tags: []string{"demo"}}) {
		t.Error("--tags=demo should allow")
	}
	if tagsAllow(task, RunOptions{Tags: []string{"other"}}) {
		t.Error("--tags=other should skip")
	}
	if tagsAllow(task, RunOptions{SkipTags: []string{"demo"}}) {
		t.Error("--skip-tags should skip")
	}
	always := PlayTask{Name: "a", Tags: []string{"always"}}
	if !tagsAllow(always, RunOptions{Tags: []string{"other"}}) {
		t.Error("always should run")
	}
	if tagsAllow(always, RunOptions{SkipTags: []string{"always"}}) {
		t.Error("skip should beat always")
	}
}

func TestSplitBatches(t *testing.T) {
	hosts := []Host{{Addr: "a"}, {Addr: "b"}, {Addr: "c"}}
	if b := splitBatches(hosts, 0); len(b) != 1 || len(b[0]) != 3 {
		t.Errorf("serial=0: %v", b)
	}
	if b := splitBatches(hosts, 2); len(b) != 2 || len(b[0]) != 2 || len(b[1]) != 1 {
		t.Errorf("serial=2: %v", b)
	}
}

func TestDeriveFamily(t *testing.T) {
	for in, want := range map[string]string{
		"debian": "debian", "ubuntu": "debian",
		"fedora": "redhat", "centos": "redhat",
		"alpine": "alpine", "arch": "arch",
		"opensuse-leap": "suse", "gentoo": "gentoo",
		"something-else": "something-else",
	} {
		if got := deriveFamily(in); got != want {
			t.Errorf("deriveFamily(%q)=%q want %q", in, got, want)
		}
	}
}

func TestVaultRoundtrip(t *testing.T) {
	ct, err := encryptVault("secret-value", "pw123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, vaultHeader) {
		t.Errorf("missing header: %q", ct)
	}
	pt, err := decryptVault(strings.TrimSpace(ct), "pw123")
	if err != nil {
		t.Fatal(err)
	}
	if pt != "secret-value" {
		t.Errorf("got %q", pt)
	}
	if _, err := decryptVault(strings.TrimSpace(ct), "wrong"); err == nil {
		t.Error("expected error with wrong password")
	}
}
