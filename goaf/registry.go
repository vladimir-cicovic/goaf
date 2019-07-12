package main

import (
	"fmt"
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
		return CopyModule{Src: src, Dest: dest}, nil
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
			if k != "src" && k != "dest" {
				vars[k] = v
			}
		}
		return TemplateModule{Src: src, Dest: dest, Vars: vars}, nil
	},
}

// LookupModule returns the factory for the given module name, or false if not found.
func LookupModule(name string) (ModuleFactory, bool) {
	f, ok := moduleRegistry[name]
	return f, ok
}
