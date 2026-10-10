package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// varsForHost merges group vars (sorted group order, later groups win) and
// per-host vars (win over groups) for a resolved host.
func (inv *Inventory) varsForHost(h Host) map[string]string {
	vars := map[string]string{}
	if len(inv.Groups) == 0 && len(inv.Hosts) == 0 {
		return vars
	}
	names := make([]string, 0, len(inv.Groups))
	for n := range inv.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	label := h.Addr
	if h.Port != 0 && h.Port != 22 {
		label = h.Addr + ":" + strconv.Itoa(h.Port)
	}
	for _, n := range names {
		raws, err := inv.collectGroup(n, map[string]bool{}, map[string]bool{})
		if err != nil {
			continue
		}
		for _, raw := range raws {
			if raw == label || raw == h.Addr {
				for k, v := range inv.Groups[n].Vars {
					vars[k] = v
				}
				break
			}
		}
	}
	if e, ok := inv.hostEntry(label, h.Addr); ok {
		for k, v := range e.Vars {
			vars[k] = v
		}
	}
	return vars
}

// Group describes a named group — direct hosts, child groups, or both.
// Vars apply to every host in the group (play vars override them).
type Group struct {
	Hosts    []string          `yaml:"hosts"`
	Children []string          `yaml:"children"`
	Vars     map[string]string `yaml:"vars"`
}

// HostEntry holds per-host variables and optional connection overrides.
type HostEntry struct {
	Vars       map[string]string `yaml:"vars"`
	User       string            `yaml:"user"`
	Port       int               `yaml:"port"`
	Key        string            `yaml:"key"`
	Connection string            `yaml:"connection"` // "ssh" (default) or "winrm"
	Password   string            `yaml:"password"`   // WinRM password (vault values allowed)
}

// Inventory describes server groups, per-host entries and global variables.
type Inventory struct {
	Groups map[string]Group     `yaml:"groups"`
	Hosts  map[string]HostEntry `yaml:"hosts"`
	Vars   struct {
		User       string `yaml:"user"`
		Port       int    `yaml:"port"`
		Key        string `yaml:"key"`
		JumpHost   string `yaml:"jump_host"`
		JumpPort   int    `yaml:"jump_port"`
		JumpUser   string `yaml:"jump_user"`
		Connection string `yaml:"connection"` // default connection for all hosts
		Password   string `yaml:"password"`   // default WinRM password (vault allowed)
	} `yaml:"vars"`
}

// Host is a resolved target with all connection parameters.
type Host struct {
	Addr       string
	User       string
	Port       int
	Key        string // path to private key; empty = use agent / default keys
	JumpAddr   string // empty = direct connection, non-empty = connect via jump host
	JumpUser   string
	JumpPort   int
	Connection string // "ssh" (default) or "winrm"
	Password   string // WinRM password (vault values decrypted at load)
}

// LoadInventory reads and parses a YAML inventory file from the given path.
func LoadInventory(path string) (*Inventory, error) {
	// Dynamic inventory: `exec:<command>` runs a command and parses its
	// stdout as inventory YAML/JSON. An existing non-YAML executable file
	// (script) is executed the same way.
	if strings.HasPrefix(path, "exec:") {
		return loadInventoryExec(strings.TrimPrefix(path, "exec:"))
	}
	if st, err := os.Stat(path); err == nil && !st.IsDir() && !isInventoryFile(path) && isExecutableFile(path) {
		return loadInventoryExec(scriptCommand(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseInventoryBytes(data)
}

// loadInventoryExec runs a command and parses its stdout as inventory.
func loadInventoryExec(command string) (*Inventory, error) {
	out, err := runInventoryCommand(command)
	if err != nil {
		return nil, fmt.Errorf("dynamic inventory %q: %w: %s", command, err, strings.TrimSpace(out))
	}
	return parseInventoryBytes([]byte(out))
}

// runInventoryCommand executes command through the system shell and
// returns combined stdout (stderr is appended to the error only).
func runInventoryCommand(command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	out, err := cmd.Output()
	return string(out), err
}

// isInventoryFile reports YAML/JSON inventory files (parsed, not executed).
func isInventoryFile(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".json")
}

// isExecutableFile reports scripts runnable as dynamic inventory.
func isExecutableFile(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range []string{".sh", ".bat", ".cmd", ".ps1", ".py", ".exe"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	if st, err := os.Stat(path); err == nil {
		return st.Mode()&0o111 != 0
	}
	return false
}

// shellQuote quotes one local shell word (POSIX single quotes, double
// quotes on Windows — cmd.exe does not understand single quotes).
// NOTE: only for control-node paths; remote commands use shQuote.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return `"` + s + `"`
	}
	return shQuote(s)
}

// scriptCommand builds the shell command running a script file.
// PowerShell scripts need powershell.exe explicitly, even on Windows.
// NOTE: on Windows the path stays unquoted for cmd.exe scripts — Go escapes
// embedded double quotes with backslashes, which cmd.exe cannot parse
// (powershell.exe understands them, so .ps1 stays quoted). Consequence:
// script paths with spaces do not work on Windows; use short paths.
func scriptCommand(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ".ps1") {
		return "powershell -NoProfile -ExecutionPolicy Bypass -File " + shellQuote(path)
	}
	if runtime.GOOS == "windows" {
		return path
	}
	return shellQuote(path)
}

func parseInventoryBytes(data []byte) (*Inventory, error) {
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, err
	}
	// Decrypt $GOAFVAULT passwords (global + host entries) when possible.
	if strings.HasPrefix(inv.Vars.Password, vaultHeader) {
		if dv, err := decryptVaultValue(inv.Vars.Password, vaultPassFile, vaultAskPass); err == nil {
			inv.Vars.Password = dv
		}
	}
	for key, e := range inv.Hosts {
		if strings.HasPrefix(e.Password, vaultHeader) {
			if dv, err := decryptVaultValue(e.Password, vaultPassFile, vaultAskPass); err == nil {
				e.Password = dv
				inv.Hosts[key] = e
			}
		}
	}
	// Decrypt $GOAFVAULT group/host vars when a password is available
	// (same sources as playbook vars); otherwise envelopes fail clearly
	// when actually used.
	for name, g := range inv.Groups {
		decrypted, err := decryptVarMap("inventory group "+name, g.Vars)
		if err != nil {
			return nil, err
		}
		g.Vars = decrypted
		inv.Groups[name] = g
	}
	for key, e := range inv.Hosts {
		decrypted, err := decryptVarMap("inventory host "+key, e.Vars)
		if err != nil {
			return nil, err
		}
		e.Vars = decrypted
		inv.Hosts[key] = e
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

// hostEntry returns the per-host entry for a raw host string ("addr" or
// "addr:port"), matching the full string first, then the bare address.
func (inv *Inventory) hostEntry(raw, addr string) (HostEntry, bool) {
	if e, ok := inv.Hosts[raw]; ok {
		return e, true
	}
	if e, ok := inv.Hosts[addr]; ok {
		return e, true
	}
	return HostEntry{}, false
}

func (inv *Inventory) parseHosts(rawHosts []string) ([]Host, error) {
	hosts := make([]Host, 0, len(rawHosts))
	for _, raw := range rawHosts {
		h := Host{User: inv.Vars.User, Port: inv.Vars.Port, Key: inv.Vars.Key,
			Connection: inv.Vars.Connection, Password: inv.Vars.Password}
		if h.Connection == "" {
			h.Connection = "ssh"
		}
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
		if e, ok := inv.hostEntry(raw, h.Addr); ok {
			if e.User != "" {
				h.User = e.User
			}
			if e.Port != 0 {
				h.Port = e.Port
			}
			if e.Key != "" {
				h.Key = e.Key
			}
			if e.Connection != "" {
				h.Connection = e.Connection
			}
			if e.Password != "" {
				h.Password = e.Password
			}
		}
		// WinRM defaults to port 5985 unless a port was given explicitly.
		if h.Connection == "winrm" && h.Port == 22 {
			h.Port = 5985
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
