package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

type Play struct {
	Name        string
	Hosts       string
	Become      bool
	Vars        map[string]string
	PreTasks    []PlayTask // run before Tasks; handlers flush after each section
	Tasks       []PlayTask
	PostTasks   []PlayTask // run after Tasks
	Handlers    []PlayTask
	GatherFacts bool // true = gather host facts before tasks (default true)
	// MaxFailPct aborts the whole run when more than this % of batch hosts
	// fail (nil = no limit). AnyErrorsFatal aborts on the first failure.
	MaxFailPct     *int `yaml:"-"`
	AnyErrorsFatal bool `yaml:"-"`
	// FileDir is the playbook's directory (resolves relative includes).
	// RolesPath is the base directory for roles (flag --roles-path).
	FileDir   string `yaml:"-"`
	RolesPath string `yaml:"-"`
	// Outputs are evaluated once per play (first host's vars) and printed.
	Outputs map[string]string `yaml:"output"`
}

type PlayTask struct {
	Name        string
	Module      string // "block" for block items (see Block/Rescue/Always)
	Params      map[string]string
	When        string     // Go template expression; skip task if evaluates to false/0/no/empty
	Loop        []string   // iterate task over each item, available as {{.item}}
	Notify      string     // handler name to trigger if this task changed something
	Register    string     // save task output into a per-host variable for later tasks
	FailedWhen  string     // Go template over vars+facts+result; mark failed when true
	ChangedWhen string     // Go template over vars+facts+result; override changed when set
	IgnoreErrs  bool       // continue the play when this task fails
	Tags        []string   // task tags for --tags/--skip-tags filtering
	Block       []PlayTask // block: section (Module == "block")
	Rescue      []PlayTask // run on hosts where block tasks failed
	Always      []PlayTask // always run, regardless of block/rescue outcome
	RunOnce     bool       // run only on the first host
	DelegateTo  string     // "localhost" runs a command task locally (other modules error)
	Retries     int        // extra attempts after the first (total = 1 + retries)
	Delay       int        // seconds between retries
	Until       string     // Go template over vars+facts+result; retry until true
	RoleDir     string     // set for tasks from a role (resolves files//templates/)
}

var knownModuleNames = []string{
	"command", "package", "install", "remove",
	"copy", "file", "service", "template", "setup",
	"user", "lineinfile", "authorized_key", "reboot", "upgrade",
	"debug", "set_fact", "script", "fetch", "meta",
}

func loadPlaybook(path string) ([]Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadPlaybookBytes(data, filepath.Dir(path), rolesPath)
}

func loadPlaybookBytes(data []byte, dir, rolesDir string) ([]Play, error) {
	var rawPlays []struct {
		Name        string                   `yaml:"name"`
		Hosts       string                   `yaml:"hosts"`
		Become      bool                     `yaml:"become"`
		Vars        map[string]string        `yaml:"vars"`
		GatherFacts *bool                    `yaml:"gather_facts"`
		PreTasks    []map[string]interface{} `yaml:"pre_tasks"`
		Tasks       []map[string]interface{} `yaml:"tasks"`
		PostTasks   []map[string]interface{} `yaml:"post_tasks"`
		Handlers    []map[string]interface{} `yaml:"handlers"`
		Roles       []interface{}            `yaml:"roles"`
		MaxFailPct  *int                     `yaml:"max_fail_percentage"`
		AnyErrors   *bool                    `yaml:"any_errors_fatal"`
		Outputs     map[string]string        `yaml:"output"`
	}

	if err := yaml.Unmarshal(data, &rawPlays); err != nil {
		return nil, fmt.Errorf("parsing playbook: %w", err)
	}

	plays := make([]Play, 0, len(rawPlays))
	for _, rp := range rawPlays {
		preTasks, err := parseTasks(rp.PreTasks, dir)
		if err != nil {
			return nil, fmt.Errorf("play %q pre_tasks: %w", rp.Name, err)
		}
		tasks, err := parseTasks(rp.Tasks, dir)
		if err != nil {
			return nil, fmt.Errorf("play %q: %w", rp.Name, err)
		}
		postTasks, err := parseTasks(rp.PostTasks, dir)
		if err != nil {
			return nil, fmt.Errorf("play %q post_tasks: %w", rp.Name, err)
		}
		handlers, err := parseTasks(rp.Handlers, dir)
		if err != nil {
			return nil, fmt.Errorf("play %q handlers: %w", rp.Name, err)
		}
		// Roles run before tasks; role handlers join play handlers.
		roleTasks, roleHandlers, roleVars, err := expandRoles(rp.Name, rp.Roles, dir, rolesDir)
		if err != nil {
			return nil, err
		}
		tasks = append(roleTasks, tasks...)
		handlers = append(handlers, roleHandlers...)
		gatherFacts := true
		if rp.GatherFacts != nil {
			gatherFacts = *rp.GatherFacts
		}
		// Decrypt $GOAFVAULT play vars up front so {{.var}} references
		// expand to plaintext. Without a password the envelope is kept
		// and fails later with a clear error when actually used.
		vars, err := decryptVarMap(rp.Name, rp.Vars)
		if err != nil {
			return nil, err
		}
		// Role vars win over play vars (explicit role > playbook).
		for k, v := range roleVars {
			vars[k] = v
		}
		anyFatal := false
		if rp.AnyErrors != nil {
			anyFatal = *rp.AnyErrors
		}
		plays = append(plays, Play{
			Name:           rp.Name,
			Hosts:          rp.Hosts,
			Become:         rp.Become,
			Vars:           vars,
			PreTasks:       preTasks,
			Tasks:          tasks,
			PostTasks:      postTasks,
			Handlers:       handlers,
			GatherFacts:    gatherFacts,
			MaxFailPct:     rp.MaxFailPct,
			AnyErrorsFatal: anyFatal,
			Outputs:        rp.Outputs,
		})
	}
	return plays, nil
}

func parseTasks(rawTasks []map[string]interface{}, baseDir string) ([]PlayTask, error) {
	tasks := make([]PlayTask, 0, len(rawTasks))
	for _, raw := range rawTasks {
		task, err := parseOneTask(raw, baseDir)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// decryptVarMap decrypts $GOAFVAULT values when a password is available,
// otherwise keeps the envelopes (they fail clearly when actually used).
func decryptVarMap(playName string, vars map[string]string) (map[string]string, error) {
	hasVault := false
	for _, v := range vars {
		if strings.HasPrefix(v, vaultHeader) {
			hasVault = true
			break
		}
	}
	if !hasVault {
		return vars, nil
	}
	pw, err := vaultPassword(vaultPassFile, vaultAskPass)
	if err != nil {
		return vars, nil
	}
	decrypted := make(map[string]string, len(vars))
	for k, v := range vars {
		if strings.HasPrefix(v, vaultHeader) {
			dv, derr := decryptVault(v, pw)
			if derr != nil {
				return nil, fmt.Errorf("play %q var %q: %w", playName, k, derr)
			}
			decrypted[k] = dv
		} else {
			decrypted[k] = v
		}
	}
	return decrypted, nil
}

// resolveRoleFile maps a relative copy/template/script src to a role file:
// templates/<src> for templates, files/<src> for copy/script, then
// <role>/<src>; absolute paths and missing files pass through (the module
// errors clearly). First existing candidate wins.
func resolveRoleFile(roleDir, module, src string) string {
	if roleDir == "" || filepath.IsAbs(src) {
		return src
	}
	var cands []string
	switch module {
	case "template":
		cands = []string{filepath.Join(roleDir, "templates", src)}
	case "copy", "script":
		cands = []string{filepath.Join(roleDir, "files", src)}
	}
	cands = append(cands, filepath.Join(roleDir, src))
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return src
}

// resolveInclude finds an included task file: relative to baseDir, then CWD.
func resolveInclude(baseDir, file string) (string, error) {
	if filepath.IsAbs(file) {
		if _, err := os.Stat(file); err == nil {
			return file, nil
		}
		return "", fmt.Errorf("include_tasks: file not found: %s", file)
	}
	for _, cand := range []string{filepath.Join(baseDir, file), file} {
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("include_tasks: file not found: %s (looked in %s)", file, baseDir)
}

// rolesPath is the base directory for roles (flag --roles-path).
var rolesPath = "roles"

// resolveRoleDir finds roles/<name>: under rolesDir, then under playDir/rolesDir.
func resolveRoleDir(name, playDir, rolesDir string) (string, error) {
	candidates := []string{filepath.Join(rolesDir, name)}
	if playDir != "" && playDir != "." {
		candidates = append(candidates, filepath.Join(playDir, rolesDir, name))
	}
	tasksFile := ""
	for _, dir := range candidates {
		tasksFile = filepath.Join(dir, "tasks", "main.yml")
		if _, err := os.Stat(tasksFile); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("role %q not found (looked in %s)", name, strings.Join(candidates, ", "))
}

// loadRoleFileMap reads an optional role map file (vars/main.yml,
// defaults/main.yml); missing file means empty map.
func loadRoleFileMap(roleDir, file string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(roleDir, file))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Join(roleDir, file), err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// markRoleDir tags tasks (recursively) with their role directory so
// copy/template/script resolve files/ and templates/ correctly.
func markRoleDir(tasks []PlayTask, dir string) {
	for i := range tasks {
		tasks[i].RoleDir = dir
		markRoleDir(tasks[i].Block, dir)
		markRoleDir(tasks[i].Rescue, dir)
		markRoleDir(tasks[i].Always, dir)
	}
}

// expandRoles loads roles for a play: role tasks run before play tasks,
// role handlers join play handlers, role vars win over play vars.
// Entry forms: "name" or {role: name, key: value, ...} (extra keys = vars).
func expandRoles(playName string, entries []interface{}, playDir, rolesDir string) (tasks, handlers []PlayTask, vars map[string]string, err error) {
	vars = map[string]string{}
	for _, e := range entries {
		name := ""
		params := map[string]string{}
		switch v := e.(type) {
		case string:
			name = v
		case map[string]interface{}:
			rn, _ := v["role"].(string)
			if rn == "" {
				return nil, nil, nil, fmt.Errorf("play %q: role entry needs a 'role' name", playName)
			}
			name = rn
			for k, val := range v {
				if k == "role" {
					continue
				}
				params[k] = fmt.Sprintf("%v", val)
			}
		default:
			return nil, nil, nil, fmt.Errorf("play %q: bad role entry %v", playName, e)
		}
		dir, rerr := resolveRoleDir(name, playDir, rolesDir)
		if rerr != nil {
			return nil, nil, nil, fmt.Errorf("play %q: %w", playName, rerr)
		}
		rawList, rerr := parseTaskListFile(filepath.Join(dir, "tasks", "main.yml"))
		if rerr != nil {
			return nil, nil, nil, fmt.Errorf("play %q: %w", playName, rerr)
		}
		roleTasks, rerr := parseTasks(rawList, filepath.Join(dir, "tasks"))
		if rerr != nil {
			return nil, nil, nil, fmt.Errorf("play %q role %q: %w", playName, name, rerr)
		}
		// Prefix names like Ansible ("role : task") for readable output.
		for i := range roleTasks {
			roleTasks[i].Name = name + " : " + roleTasks[i].Name
		}
		markRoleDir(roleTasks, dir)
		var roleHandlers []PlayTask
		if _, serr := os.Stat(filepath.Join(dir, "handlers", "main.yml")); serr == nil {
			rawH, rerr := parseTaskListFile(filepath.Join(dir, "handlers", "main.yml"))
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("play %q: %w", playName, rerr)
			}
			roleHandlers, rerr = parseTasks(rawH, filepath.Join(dir, "handlers"))
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("play %q role %q handlers: %w", playName, name, rerr)
			}
			markRoleDir(roleHandlers, dir)
		}
		defs, rerr := loadRoleFileMap(dir, filepath.Join("defaults", "main.yml"))
		if rerr != nil {
			return nil, nil, nil, fmt.Errorf("play %q: %w", playName, rerr)
		}
		rvars, rerr := loadRoleFileMap(dir, filepath.Join("vars", "main.yml"))
		if rerr != nil {
			return nil, nil, nil, fmt.Errorf("play %q: %w", playName, rerr)
		}
		// defaults lose to everything set so far; vars + params win.
		for k, v := range defs {
			if _, ok := vars[k]; !ok {
				vars[k] = v
			}
		}
		rvars, rerr = decryptVarMap(playName+" role "+name, rvars)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		for k, v := range rvars {
			vars[k] = v
		}
		for k, v := range params {
			vars[k] = v
		}
		tasks = append(tasks, roleTasks...)
		handlers = append(handlers, roleHandlers...)
	}
	return tasks, handlers, vars, nil
}

// parseTaskListFile reads a YAML task list file (tasks or handlers).
func parseTaskListFile(path string) ([]map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []map[string]interface{}
	if err := yaml.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return list, nil
}

func parseOneTask(raw map[string]interface{}, baseDir string) (PlayTask, error) {
	name, _ := raw["name"].(string)
	notify, _ := raw["notify"].(string)
	register, _ := raw["register"].(string)

	// when / failed_when / changed_when may be bool literals or string templates
	strVal := func(key string) string {
		if w, ok := raw[key]; ok {
			return fmt.Sprintf("%v", w)
		}
		return ""
	}
	when := strVal("when")
	failedWhen := strVal("failed_when")
	changedWhen := strVal("changed_when")

	ignoreErrs := false
	if v, ok := raw["ignore_errors"]; ok {
		ignoreErrs = fmt.Sprintf("%v", v) == "true"
	}

	runOnce := false
	if v, ok := raw["run_once"]; ok {
		runOnce = fmt.Sprintf("%v", v) == "true"
	}
	delegateTo, _ := raw["delegate_to"].(string)
	retries, delay := 0, 0
	if v, ok := raw["retries"]; ok {
		fmt.Sscanf(fmt.Sprintf("%v", v), "%d", &retries)
		if retries < 0 {
			retries = 0
		}
	}
	if v, ok := raw["delay"]; ok {
		fmt.Sscanf(fmt.Sprintf("%v", v), "%d", &delay)
		if delay < 0 {
			delay = 0
		}
	}
	until := strVal("until")

	// tags — single string or list (parsed early: blocks use it too)
	var tags []string
	if v, ok := raw["tags"]; ok {
		if list, ok := v.([]interface{}); ok {
			for _, item := range list {
				tags = append(tags, fmt.Sprintf("%v", item))
			}
		} else {
			tags = append(tags, fmt.Sprintf("%v", v))
		}
	}

	// include_tasks: splice an external task file (as a block).
	if iv, ok := raw["include_tasks"]; ok {
		incFile, ok := iv.(string)
		if !ok {
			return PlayTask{}, fmt.Errorf("task %q: include_tasks must be a file path", name)
		}
		resolved, err := resolveInclude(baseDir, incFile)
		if err != nil {
			return PlayTask{}, fmt.Errorf("task %q: %w", name, err)
		}
		rawList, err := parseTaskListFile(resolved)
		if err != nil {
			return PlayTask{}, fmt.Errorf("task %q: %w", name, err)
		}
		included, err := parseTasks(rawList, filepath.Dir(resolved))
		if err != nil {
			return PlayTask{}, fmt.Errorf("task %q: %w", name, err)
		}
		return PlayTask{
			Name:   name,
			Module: "block",
			When:   when,
			Tags:   tags,
			Block:  included,
		}, nil
	}

	// block item: {name, block: [...], rescue: [...], always: [...]}
	if bv, ok := raw["block"]; ok {
		block, err := parseSubTasks(name, "block", bv, baseDir)
		if err != nil {
			return PlayTask{}, err
		}
		rescue, err := parseSubTasks(name, "rescue", raw["rescue"], baseDir)
		if err != nil {
			return PlayTask{}, err
		}
		always, err := parseSubTasks(name, "always", raw["always"], baseDir)
		if err != nil {
			return PlayTask{}, err
		}
		if len(block) == 0 {
			return PlayTask{}, fmt.Errorf("task %q: block is empty", name)
		}
		return PlayTask{
			Name:       name,
			Module:     "block",
			When:       when,
			Tags:       tags,
			Block:      block,
			Rescue:     rescue,
			Always:     always,
			RunOnce:    runOnce,
			DelegateTo: delegateTo,
		}, nil
	}

	// loop / with_items — list of items to iterate over
	var loop []string
	for _, key := range []string{"loop", "with_items"} {
		if v, ok := raw[key]; ok {
			items, ok := v.([]interface{})
			if !ok {
				return PlayTask{}, fmt.Errorf("task %q: %q must be a list", name, key)
			}
			for _, item := range items {
				loop = append(loop, fmt.Sprintf("%v", item))
			}
			break
		}
	}

	for _, modName := range knownModuleNames {
		val, ok := raw[modName]
		if !ok {
			continue
		}
		params, err := taskParams(modName, val)
		if err != nil {
			return PlayTask{}, fmt.Errorf("task %q: %w", name, err)
		}
		// meta is structural (flush_handlers), not a remote module.
		if modName == "meta" {
			return PlayTask{
				Name:   name,
				Module: "meta",
				Params: params,
				When:   when,
				Tags:   tags,
			}, nil
		}
		return PlayTask{
			Name:        name,
			Module:      modName,
			Params:      params,
			When:        when,
			Loop:        loop,
			Notify:      notify,
			Register:    register,
			FailedWhen:  failedWhen,
			ChangedWhen: changedWhen,
			IgnoreErrs:  ignoreErrs,
			Tags:        tags,
			RunOnce:     runOnce,
			DelegateTo:  delegateTo,
			Retries:     retries,
			Delay:       delay,
			Until:       until,
		}, nil
	}
	return PlayTask{}, fmt.Errorf("task %q: no known module key found (expected one of: %s)",
		name, strings.Join(knownModuleNames, ", "))
}

// parseSubTasks parses a block/rescue/always sub-list (nil when absent).
func parseSubTasks(taskName, key string, v interface{}, baseDir string) ([]PlayTask, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("task %q: %q must be a list", taskName, key)
	}
	var out []PlayTask
	for _, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("task %q: %q entries must be maps", taskName, key)
		}
		sub, err := parseOneTask(m, baseDir)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

// evalWhen expands the when expression as a Go template using missingkey=zero
// (missing variables evaluate to empty string → false) then checks the result.
// Returns false for "", "false", "0", "no", "<no value>" (case-insensitive).
func evalWhen(expr string, vars map[string]string) (bool, error) {
	data := make(map[string]interface{}, len(vars))
	for k, v := range vars {
		data[k] = v
	}
	tmpl, err := template.New("when").Option("missingkey=zero").Parse(expr)
	if err != nil {
		return false, fmt.Errorf("when %q: parse error: %w", expr, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("when %q: eval error: %w", expr, err)
	}
	val := strings.TrimSpace(buf.String())
	switch strings.ToLower(val) {
	case "", "false", "0", "no", "<no value>":
		return false, nil
	default:
		return true, nil
	}
}

// mergeVars returns a new map that is base extended by extra (extra wins on conflict).
func mergeVars(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

// taskParams converts a YAML value to a string param map.
// Supports shorthand string value for command, package/install/remove.
func taskParams(modName string, val interface{}) (map[string]string, error) {
	switch v := val.(type) {
	case string:
		switch modName {
		case "command":
			return map[string]string{"cmd": v}, nil
		case "package", "install", "remove":
			return map[string]string{"name": v}, nil
		case "meta":
			return map[string]string{"action": v}, nil
		case "debug", "set_fact":
			// space-separated key=value pairs, e.g. "var=osline" or "a=1 b=2"
			m := make(map[string]string)
			for _, field := range strings.Fields(v) {
				idx := strings.Index(field, "=")
				if idx <= 0 {
					return nil, fmt.Errorf("module %q: bad pair %q, want key=value", modName, field)
				}
				m[field[:idx]] = field[idx+1:]
			}
			return m, nil
		}
		return nil, fmt.Errorf("module %q does not support shorthand string value", modName)
	case map[string]interface{}:
		m := make(map[string]string, len(v))
		for k, val := range v {
			m[k] = fmt.Sprintf("%v", val)
		}
		return m, nil
	}
	return nil, fmt.Errorf("unexpected param type %T for module %q", val, modName)
}

// vaultPassFile / vaultAskPass configure vault decryption for $GOAFVAULT
// values in playbook vars (set from CLI flags).
var vaultPassFile string
var vaultAskPass bool

// workspaceName / workspaceVars select environment variables:
// --workspace=name loads workspaces/<name>.yml, merged under play vars.
// "workspace" itself is always available as a variable (default "default").
var workspaceName = "default"
var workspaceVars = map[string]string{"workspace": "default"}

// workspacesDir is the base directory for workspaces (flag --workspaces-dir).
var workspacesDir = "workspaces"

// setupWorkspace loads the requested workspace file into workspaceVars.
// Unknown workspace (other than default) is an error; "default" without a
// file just sets the workspace name.
func setupWorkspace(name string) error {
	if name == "" {
		name = "default"
	}
	workspaceName = name
	workspaceVars = map[string]string{"workspace": name}
	if name == "default" {
		if _, err := os.Stat(filepath.Join(workspacesDir, "default.yml")); err != nil {
			return nil // default workspace needs no file
		}
	}
	data, err := os.ReadFile(filepath.Join(workspacesDir, name+".yml"))
	if err != nil {
		return fmt.Errorf("workspace %q: %w", name, err)
	}
	// Nested {vars: {...}} form first, flat key=value map otherwise.
	var doc map[string]string
	if nested, ok := rawVarsKey(data); ok {
		doc = nested
	} else {
		doc = map[string]string{}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("workspace %q: %w", name, err)
		}
	}
	decrypted, err := decryptVarMap("workspace "+name, doc)
	if err != nil {
		return err
	}
	for k, v := range decrypted {
		workspaceVars[k] = v
	}
	workspaceVars["workspace"] = name
	return nil
}

// rawVarsKey extracts a top-level "vars:" map when present.
func rawVarsKey(data []byte) (map[string]string, bool) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false
	}
	v, ok := doc["vars"]
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = fmt.Sprintf("%v", val)
	}
	return out, true
}

// expandVars renders Go template expressions in each param value using vars.
// $GOAFVAULT values are decrypted first (password via --vault-pass-file,
// --ask-vault-pass or GOAF_VAULT_PASSWORD).
func expandVars(params map[string]string, vars map[string]string) (map[string]string, error) {
	if len(vars) == 0 {
		return params, nil
	}
	data := make(map[string]interface{}, len(vars))
	for k, v := range vars {
		data[k] = v
	}
	result := make(map[string]string, len(params))
	for k, v := range params {
		if strings.HasPrefix(v, vaultHeader) {
			dv, err := decryptVaultValue(v, vaultPassFile, vaultAskPass)
			if err != nil {
				return nil, fmt.Errorf("param %q: %w", k, err)
			}
			result[k] = dv
			continue
		}
		if !strings.Contains(v, "{{") {
			result[k] = v
			continue
		}
		tmpl, err := template.New("").Option("missingkey=error").Parse(v)
		if err != nil {
			return nil, fmt.Errorf("param %q: template parse error: %w", k, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("param %q: template expand error: %w", k, err)
		}
		result[k] = buf.String()
	}
	return result, nil
}
