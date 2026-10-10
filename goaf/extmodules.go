package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// External modules ("collections" light): executable scripts in the modules
// path become first-class modules named by file (extension stripped).
//
// Protocol (shell-friendly, no JSON needed on the target):
//   <bin> check        with params as GOAF_P_<key> environment variables
//   <bin> apply
// stdout contract, first non-empty line:
//   check: "needed: true|false"
//   apply: "changed: true|false"
// optional "error: <msg>" line (or non-zero exit) fails the run;
// any following "output: ..." lines (rest of stdout) become the result output.
//
// .py files run with python3, everything else with sh.

// ExternalModule is a script-backed module uploaded to the target on each use.
type ExternalModule struct {
	ModName string
	Bin     string // local script path
	Params  map[string]string
}

func (m ExternalModule) Name() string { return m.ModName }

// remotePath is deterministic so repeated uploads simply overwrite.
func (m ExternalModule) remotePath() string {
	return "/tmp/.goaf-ext-" + m.ModName
}

func (m ExternalModule) interpreter() string {
	if strings.HasSuffix(strings.ToLower(m.Bin), ".py") {
		return "python3"
	}
	return "sh"
}

// extResult is the parsed script answer.
type extResult struct {
	flag   bool // needed (check) or changed (apply)
	output string
	errMsg string
}

// parseExtResult parses script stdout: first non-empty line "key: value",
// wantKey is "needed" or "changed". Remaining "output:" lines (and any
// other lines after the first) form the output.
func parseExtResult(out, wantKey string) (extResult, error) {
	var res extResult
	lines := strings.Split(out, "\n")
	first := true
	var b strings.Builder
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if first {
			first = false
			kv := strings.SplitN(t, ":", 2)
			if len(kv) != 2 {
				return res, fmt.Errorf("bad first line %q, want %q: true|false", t, wantKey)
			}
			key := strings.TrimSpace(kv[0])
			val := strings.TrimSpace(kv[1])
			if strings.HasPrefix(val, "error") || key == "error" {
				res.errMsg = strings.TrimSpace(strings.TrimPrefix(val, "error"))
				if res.errMsg == "" {
					res.errMsg = strings.TrimSpace(strings.TrimPrefix(t, "error:"))
				}
				return res, nil
			}
			if key != wantKey {
				return res, fmt.Errorf("bad first line %q, want %q: true|false", t, wantKey)
			}
			switch val {
			case "true":
				res.flag = true
			case "false":
				res.flag = false
			default:
				return res, fmt.Errorf("bad value %q, want true|false", val)
			}
			continue
		}
		if rest, ok := strings.CutPrefix(t, "output:"); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(strings.TrimSpace(rest))
		} else {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t)
		}
	}
	res.output = b.String()
	return res, nil
}

// upload copies the script to the remote host (overwrite each time).
func (m ExternalModule) upload(s Remote) error {
	if err := s.Upload(m.Bin, m.remotePath()); err != nil {
		return fmt.Errorf("uploading external module %q: %w", m.ModName, err)
	}
	if _, err := s.Run("chmod +x " + shQuote(m.remotePath())); err != nil {
		return fmt.Errorf("chmod external module %q: %w", m.ModName, err)
	}
	return nil
}

// invoke runs the remote script with a verb and params as GOAF_P_* env.
func (m ExternalModule) invoke(s Remote, verb, wantKey string) (extResult, error) {
	if err := m.upload(s); err != nil {
		return extResult{}, err
	}
	var b strings.Builder
	keys := make([]string, 0, len(m.Params))
	for k := range m.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("GOAF_P_" + k + "=" + shQuote(m.Params[k]) + " ")
	}
	cmd := b.String() + m.interpreter() + " " + shQuote(m.remotePath()) + " " + verb
	out, err := s.Run(cmd)
	if err != nil {
		return extResult{}, fmt.Errorf("external module %q %s: %w: %s", m.ModName, verb, err, strings.TrimSpace(out))
	}
	res, perr := parseExtResult(out, wantKey)
	if perr != nil {
		return extResult{}, fmt.Errorf("external module %q %s: %w (output: %q)", m.ModName, verb, perr, strings.TrimSpace(out))
	}
	if res.errMsg != "" {
		return extResult{}, fmt.Errorf("external module %q: %s", m.ModName, res.errMsg)
	}
	return res, nil
}

func (m ExternalModule) Check(s Remote) (bool, error) {
	if isWinRM(s) {
		return false, fmt.Errorf("external modules are not supported over WinRM yet (POSIX shell only)")
	}
	res, err := m.invoke(s, "check", "needed")
	if err != nil {
		return false, err
	}
	return res.flag, nil
}

func (m ExternalModule) Apply(s Remote) (string, error) {
	res, err := m.invoke(s, "apply", "changed")
	if err != nil {
		return "", err
	}
	if res.output != "" {
		return res.output, nil
	}
	return fmt.Sprintf("%s applied", m.ModName), nil
}

// externalModuleNames tracks modules from RegisterExternalModules so CLI
// parsing accepts any key=value pair for them.
var externalModuleNames = map[string]bool{}

// RegisterExternalModules scans dir for executable scripts and registers
// each as a module (file name without extension). Missing default dir is
// fine; an explicitly requested unreadable dir is an error.
func RegisterExternalModules(dir string, explicit bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if explicit {
			return fmt.Errorf("modules path %q: %w", dir, err)
		}
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		modName := name
		if idx := strings.LastIndex(name, "."); idx > 0 {
			modName = name[:idx]
		}
		if _, ok := moduleRegistry[modName]; ok {
			continue // builtin wins over external
		}
		bin := filepath.Join(dir, name)
		moduleRegistry[modName] = externalFactory(modName, bin)
		externalModuleNames[modName] = true
		knownModuleNames = append(knownModuleNames, modName)
	}
	sort.Strings(knownModuleNames)
	return nil
}

// externalFactory builds ExternalModules with call-time params.
func externalFactory(modName, bin string) ModuleFactory {
	return func(p map[string]string) (Module, error) {
		cp := make(map[string]string, len(p))
		for k, v := range p {
			cp[k] = v
		}
		return ExternalModule{ModName: modName, Bin: bin, Params: cp}, nil
	}
}

// listModuleNames returns all registered module names, sorted.
func listModuleNames() []string {
	names := make([]string, 0, len(moduleRegistry))
	for n := range moduleRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
