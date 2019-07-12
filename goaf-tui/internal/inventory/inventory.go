package inventory

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

type Group struct {
	Hosts    []string `yaml:"hosts"`
	Children []string `yaml:"children"`
}

type Inventory struct {
	Groups map[string]Group `yaml:"groups"`
	Vars   struct {
		User     string `yaml:"user"`
		Port     int    `yaml:"port"`
		JumpHost string `yaml:"jump_host"`
		JumpPort int    `yaml:"jump_port"`
		JumpUser string `yaml:"jump_user"`
	} `yaml:"vars"`
}

// Item is one row in the TUI inventory tree.
type Item struct {
	Label   string
	IsGroup bool
	Indent  int
}

// Load reads and parses a YAML inventory file from the given path.
func Load(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, err
	}
	if len(inv.Groups) == 0 {
		return nil, fmt.Errorf("no groups defined in inventory")
	}
	return &inv, nil
}

// BuildTree converts an Inventory into a flat ordered list for display.
// Root groups (not referenced as children by any other group) appear first.
func BuildTree(inv *Inventory) []Item {
	childSet := map[string]bool{}
	for _, g := range inv.Groups {
		for _, c := range g.Children {
			childSet[c] = true
		}
	}

	var roots []string
	for name := range inv.Groups {
		if !childSet[name] {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)

	var items []Item
	visited := map[string]bool{}
	for _, root := range roots {
		items = append(items, groupItems(inv, root, 0, visited)...)
	}
	return items
}

func groupItems(inv *Inventory, name string, indent int, visited map[string]bool) []Item {
	if visited[name] {
		return nil
	}
	visited[name] = true

	items := []Item{{Label: name, IsGroup: true, Indent: indent}}
	g := inv.Groups[name]

	for _, child := range g.Children {
		items = append(items, groupItems(inv, child, indent+1, visited)...)
	}
	for _, host := range g.Hosts {
		items = append(items, Item{Label: host, IsGroup: false, Indent: indent + 1})
	}
	return items
}
