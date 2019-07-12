package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Group describes a named group — direct hosts, child groups, or both.
type Group struct {
	Hosts    []string `yaml:"hosts"`
	Children []string `yaml:"children"`
}

// Inventory describes server groups and global variables.
type Inventory struct {
	Groups map[string]Group `yaml:"groups"`
	Vars   struct {
		User     string `yaml:"user"`
		Port     int    `yaml:"port"`
		Key      string `yaml:"key"`
		JumpHost string `yaml:"jump_host"`
		JumpPort int    `yaml:"jump_port"`
		JumpUser string `yaml:"jump_user"`
	} `yaml:"vars"`
}

// Host is a resolved target with all connection parameters.
type Host struct {
	Addr     string
	User     string
	Port     int
	Key      string // path to private key; empty = use agent / default keys
	JumpAddr string // empty = direct connection, non-empty = connect via jump host
	JumpUser string
	JumpPort int
}

// LoadInventory reads and parses a YAML inventory file from the given path.
func LoadInventory(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, err
	}
	if inv.Vars.User == "" {
		inv.Vars.User = "root"
	}
	if inv.Vars.Port == 0 {
		inv.Vars.Port = 22
	}
	return &inv, nil
}

// defaultInventory returns an empty inventory with default connection vars.
// Used when no inventory file is present and only direct host:port targets
// are given on the command line.
func defaultInventory() *Inventory {
	inv := &Inventory{}
	inv.Vars.User = "root"
	inv.Vars.Port = 22
	return inv
}

// Resolve takes a group name, host string, or comma-separated list of either,
// and returns a deduplicated list of Hosts.
func (inv *Inventory) Resolve(target string) ([]Host, error) {
	parts := strings.Split(target, ",")
	seen := make(map[string]bool)
	visiting := make(map[string]bool)
	var rawHosts []string

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := inv.Groups[part]; ok {
			gh, err := inv.collectGroup(part, visiting, seen)
			if err != nil {
				return nil, err
			}
			rawHosts = append(rawHosts, gh...)
		} else {
			if !seen[part] {
				seen[part] = true
				rawHosts = append(rawHosts, part)
			}
		}
	}

	if len(rawHosts) == 0 {
		return nil, fmt.Errorf("target %q resolved to no hosts", target)
	}
	return inv.parseHosts(rawHosts)
}

// collectGroup recursively collects all host strings from a group and its children.
// visiting detects circular references; seen deduplicates hosts across children.
func (inv *Inventory) collectGroup(name string, visiting, seen map[string]bool) ([]string, error) {
	if visiting[name] {
		return nil, fmt.Errorf("circular reference detected in group %q", name)
	}
	g, ok := inv.Groups[name]
	if !ok {
		return nil, fmt.Errorf("group %q not found", name)
	}
	visiting[name] = true
	defer func() { visiting[name] = false }()

	var rawHosts []string
	for _, h := range g.Hosts {
		if !seen[h] {
			seen[h] = true
			rawHosts = append(rawHosts, h)
		}
	}
	for _, child := range g.Children {
		childHosts, err := inv.collectGroup(child, visiting, seen)
		if err != nil {
			return nil, err
		}
		rawHosts = append(rawHosts, childHosts...)
	}
	return rawHosts, nil
}

func (inv *Inventory) parseHosts(rawHosts []string) ([]Host, error) {
	hosts := make([]Host, 0, len(rawHosts))
	for _, raw := range rawHosts {
		h := Host{User: inv.Vars.User, Port: inv.Vars.Port, Key: inv.Vars.Key}
		if addr, portStr, err := net.SplitHostPort(raw); err == nil {
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, fmt.Errorf("invalid port in %q: %w", raw, err)
			}
			h.Addr = addr
			h.Port = port
		} else {
			h.Addr = raw
		}
		if inv.Vars.JumpHost != "" {
			h.JumpAddr = inv.Vars.JumpHost
			h.JumpPort = inv.Vars.JumpPort
			if h.JumpPort == 0 {
				h.JumpPort = 22
			}
			h.JumpUser = inv.Vars.JumpUser
			if h.JumpUser == "" {
				h.JumpUser = inv.Vars.User
			}
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}
