package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ModuleFactory creates a module instance from a parameter map.
type ModuleFactory func(params map[string]string) (Module, error)

var moduleRegistry = map[string]ModuleFactory{
	"setup": func(_ map[string]string) (Module, error) {
		return SetupModule{}, nil
	},
	"command": func(p map[string]string) (Module, error) {
		cmd, ok := p["cmd"]
		if !ok {
			return nil, fmt.Errorf("module 'command' requires parameter 'cmd'")
		}
		return CommandModule{Cmd: cmd}, nil
	},
	"package": func(p map[string]string) (Module, error) {
		name, ok := p["name"]
		if !ok {
			return nil, fmt.Errorf("module 'package' requires parameter 'name'")
		}
		return PackageModule{Pkg: name}, nil
	},
	"install": func(p map[string]string) (Module, error) {
		name, ok := p["name"]
		if !ok {
			return nil, fmt.Errorf("module 'install' requires parameter 'name'")
		}
		return PackageModule{Pkg: name}, nil
	},
	"copy": func(p map[string]string) (Module, error) {
		src, ok := p["src"]
		if !ok {
			return nil, fmt.Errorf("module 'copy' requires parameter 'src'")
		}
		dest, ok := p["dest"]
		if !ok {
			return nil, fmt.Errorf("module 'copy' requires parameter 'dest'")
		}
		return CopyModule{Src: src, Dest: dest, Backup: p["backup"] == "true"}, nil
	},
	"file": func(p map[string]string) (Module, error) {
		path, ok := p["path"]
		if !ok {
			return nil, fmt.Errorf("module 'file' requires parameter 'path'")
		}
		state := p["state"]
		if state == "" {
			state = "file"
		}
		return FileModule{
			Path:  path,
			State: state,
			Mode:  p["mode"],
			Owner: p["owner"],
			Group: p["group"],
		}, nil
	},
	"service": func(p map[string]string) (Module, error) {
		name, ok := p["name"]
		if !ok {
			return nil, fmt.Errorf("module 'service' requires parameter 'name'")
		}
		state := p["state"]
		if state == "" {
			state = "started"
		}
		mod := ServiceModule{SvcName: name, State: state}
		if v, ok := p["enabled"]; ok {
			b := strings.ToLower(v) == "true"
			mod.Enabled = &b
		}
		return mod, nil
	},
	"remove": func(p map[string]string) (Module, error) {
		name, ok := p["name"]
		if !ok {
			return nil, fmt.Errorf("module 'remove' requires parameter 'name'")
		}
		return RemoveModule{Pkg: name}, nil
	},
	"template": func(p map[string]string) (Module, error) {
		src, ok := p["src"]
		if !ok {
			return nil, fmt.Errorf("module 'template' requires parameter 'src'")
		}
		dest, ok := p["dest"]
		if !ok {
			return nil, fmt.Errorf("module 'template' requires parameter 'dest'")
		}
		vars := make(map[string]string)
		for k, v := range p {
			if k != "src" && k != "dest" && k != "backup" {
				vars[k] = v
			}
		}
		return TemplateModule{Src: src, Dest: dest, Vars: vars, Backup: p["backup"] == "true"}, nil
	},
	"user": func(p map[string]string) (Module, error) {
		name, ok := p["name"]
		if !ok {
			return nil, fmt.Errorf("module 'user' requires parameter 'name'")
		}
		state := p["state"]
		if state == "" {
			state = "present"
		}
		return UserModule{Username: name, State: state, Shell: p["shell"], Groups: p["groups"]}, nil
	},
	"lineinfile": func(p map[string]string) (Module, error) {
		path, ok := p["path"]
		if !ok {
			return nil, fmt.Errorf("module 'lineinfile' requires parameter 'path'")
		}
		line, ok := p["line"]
		if !ok {
			return nil, fmt.Errorf("module 'lineinfile' requires parameter 'line'")
		}
		state := p["state"]
		if state == "" {
			state = "present"
		}
		return LineinfileModule{Path: path, Line: line, Regexp: p["regexp"], State: state}, nil
	},
	"authorized_key": func(p map[string]string) (Module, error) {
		user, ok := p["user"]
		if !ok {
			return nil, fmt.Errorf("module 'authorized_key' requires parameter 'user'")
		}
		key, ok := p["key"]
		if !ok {
			return nil, fmt.Errorf("module 'authorized_key' requires parameter 'key'")
		}
		state := p["state"]
		if state == "" {
			state = "present"
		}
		return AuthorizedKeyModule{User: user, Key: key, State: state}, nil
	},
	"reboot": func(p map[string]string) (Module, error) {
		timeout := 300
		if v, ok := p["timeout"]; ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeout = n
			}
		}
		return RebootModule{Timeout: timeout, Msg: p["msg"]}, nil
	},
	"upgrade": func(_ map[string]string) (Module, error) {
		return UpgradeModule{}, nil
	},
	"script": func(p map[string]string) (Module, error) {
		src, ok := p["src"]
		if !ok {
			return nil, fmt.Errorf("module 'script' requires parameter 'src'")
		}
		return ScriptModule{Src: src, Args: p["args"]}, nil
	},
	"fetch": func(p map[string]string) (Module, error) {
		src, ok := p["src"]
		if !ok {
			return nil, fmt.Errorf("module 'fetch' requires parameter 'src'")
		}
		dest, ok := p["dest"]
		if !ok {
			return nil, fmt.Errorf("module 'fetch' requires parameter 'dest'")
		}
		return FetchModule{Src: src, Dest: dest}, nil
	},
	"debug": func(p map[string]string) (Module, error) {
		// Real handling happens in the runner (control-side, no SSH).
		// This stub only exists so validate accepts the module.
		return DebugModule{Var: p["var"], Msg: p["msg"]}, nil
	},
	"set_fact": func(p map[string]string) (Module, error) {
		// Real handling happens in the runner (merges into host vars).
		return SetFactModule{Vars: p}, nil
	},
}

// LookupModule returns the factory for the given module name, or false if not found.
func LookupModule(name string) (ModuleFactory, bool) {
	f, ok := moduleRegistry[name]
	return f, ok
}
