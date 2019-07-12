package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"goaf-tui/internal/tui"
)

func main() {
	invPath := flag.String("i", "", "inventory file path")
	flag.Parse()

	p := tea.NewProgram(tui.New(*invPath), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
