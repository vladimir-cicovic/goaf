package main

import (
	"bytes"
	"fmt"
	"os"
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
}

var knownModuleNames = []string{
	"command", "package", "install", "remove",
	"copy", "file", "service", "template", "setup",
	"user", "lineinfile", "authorized_key", "reboot", "upgrade",
	"debug", "set_fact", "script", "fetch",
}

func loadPlaybook(path string) ([]Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadPlaybookBytes(data)
}

func loadPlaybookBytes(data []byte) ([]Play, error) {
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
	}

	if err := yaml.Unmarshal(data, &rawPlays); err != nil {
		return nil, fmt.Errorf("parsing playbook: %w", err)
	}

	plays := make([]Play, 0, len(rawPlays))
	for _, rp := range rawPlays {
		preTasks, err := parseTasks(rp.PreTasks)
		if err != nil {
			return nil, fmt.Errorf("play %q pre_tasks: %w", rp.Name, err)
		}
		tasks, err := parseTasks(rp.Tasks)
		if err != nil {
			return nil, fmt.Errorf("play %q: %w", rp.Name, err)
		}
		postTasks, err := parseTasks(rp.PostTasks)
		if err != nil {
			return nil, fmt.Errorf("play %q post_tasks: %w", rp.Name, err)
		}
		handlers, err := parseTasks(rp.Handlers)
		if err != nil {
			return nil, fmt.Errorf("play %q handlers: %w", rp.Name, err)
		}
		gatherFacts := true
		if rp.GatherFacts != nil {
			gatherFacts = *rp.GatherFacts
		}
		// Decrypt $GOAFVAULT play vars up front so {{.var}} references
		// expand to plaintext. Without a password the envelope is kept
		// and fails later with a clear error when actually used.
		vars := rp.Vars
		hasVault := false
		for _, v := range vars {
			if strings.HasPrefix(v, vaultHeader) {
				hasVault = true
				break
			}
		}
		if hasVault {
			if pw, err := vaultPassword(vaultPassFile, vaultAskPass); err == nil {
				decrypted := make(map[string]string, len(vars))
				for k, v := range vars {
					if strings.HasPrefix(v, vaultHeader) {
						dv, derr := decryptVault(v, pw)
						if derr != nil {
							return nil, fmt.Errorf("play %q var %q: %w", rp.Name, k, derr)
						}
						decrypted[k] = dv
					} else {
						decrypted[k] = v
					}
				}
				vars = decrypted
			}
		}
		plays = append(plays, Play{
			Name:        rp.Name,
			Hosts:       rp.Hosts,
			Become:      rp.Become,
			Vars:        vars,
			PreTasks:    preTasks,
			Tasks:       tasks,
			PostTasks:   postTasks,
			Handlers:    handlers,
			GatherFacts: gatherFacts,
		})
	}
	return plays, nil
}

func parseTasks(rawTasks []map[string]interface{}) ([]PlayTask, error) {
	tasks := make([]PlayTask, 0, len(rawTasks))
	for _, raw := range rawTasks {
		task, err := parseOneTask(raw)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func parseOneTask(raw map[string]interface{}) (PlayTask, error) {
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

	// block item: {name, block: [...], rescue: [...], always: [...]}
	if bv, ok := raw["block"]; ok {
		block, err := parseSubTasks(name, "block", bv)
		if err != nil {
			return PlayTask{}, err
		}
		rescue, err := parseSubTasks(name, "rescue", raw["rescue"])
		if err != nil {
			return PlayTask{}, err
		}
		always, err := parseSubTasks(name, "always", raw["always"])
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
		}, nil
	}
	return PlayTask{}, fmt.Errorf("task %q: no known module key found (expected one of: %s)",
		name, strings.Join(knownModuleNames, ", "))
}

// parseSubTasks parses a block/rescue/always sub-list (nil when absent).
func parseSubTasks(taskName, key string, v interface{}) ([]PlayTask, error) {
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
		sub, err := parseOneTask(m)
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
