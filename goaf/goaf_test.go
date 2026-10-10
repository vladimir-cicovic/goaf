package main

import (
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
`))
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
