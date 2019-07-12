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
	Tasks       []PlayTask
	Handlers    []PlayTask
	GatherFacts bool // true = gather host facts before tasks (default true)
}

type PlayTask struct {
	Name   string
	Module string
	Params map[string]string
	When   string   // Go template expression; skip task if evaluates to false/0/no/empty
	Loop   []string // iterate task over each item, available as {{.item}}
	Notify string   // handler name to trigger if this task changed something
}

var knownModuleNames = []string{
	"command", "package", "install", "remove",
	"copy", "file", "service", "template", "setup",
}

func loadPlaybook(path string) ([]Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var rawPlays []struct {
		Name        string                   `yaml:"name"`
		Hosts       string                   `yaml:"hosts"`
		Become      bool                     `yaml:"become"`
		Vars        map[string]string        `yaml:"vars"`
		GatherFacts *bool                    `yaml:"gather_facts"`
		Tasks       []map[string]interface{} `yaml:"tasks"`
		Handlers    []map[string]interface{} `yaml:"handlers"`
	}

	if err := yaml.Unmarshal(data, &rawPlays); err != nil {
		return nil, fmt.Errorf("parsing playbook: %w", err)
	}

	plays := make([]Play, 0, len(rawPlays))
	for _, rp := range rawPlays {
		tasks, err := parseTasks(rp.Tasks)
		if err != nil {
			return nil, fmt.Errorf("play %q: %w", rp.Name, err)
		}
		handlers, err := parseTasks(rp.Handlers)
		if err != nil {
			return nil, fmt.Errorf("play %q handlers: %w", rp.Name, err)
		}
		gatherFacts := true
		if rp.GatherFacts != nil {
			gatherFacts = *rp.GatherFacts
		}
		plays = append(plays, Play{
			Name:        rp.Name,
			Hosts:       rp.Hosts,
			Become:      rp.Become,
			Vars:        rp.Vars,
			Tasks:       tasks,
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

	// when may be a bool literal (true/false) or a string template expression
	var when string
	if w, ok := raw["when"]; ok {
		when = fmt.Sprintf("%v", w)
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
			Name:   name,
			Module: modName,
			Params: params,
			When:   when,
			Loop:   loop,
			Notify: notify,
		}, nil
	}
	return PlayTask{}, fmt.Errorf("task %q: no known module key found (expected one of: %s)",
		name, strings.Join(knownModuleNames, ", "))
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

// expandVars renders Go template expressions in each param value using vars.
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
