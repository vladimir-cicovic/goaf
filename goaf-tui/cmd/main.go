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
	showHistory := flag.Bool("history", false, "list recorded runs and exit")
	flag.Parse()

	if *showHistory {
		runs, err := tui.ListHistory()
		if err != nil {
			fmt.Fprintln(os.Stderr, "history:", err)
			os.Exit(1)
		}
		if len(runs) == 0 {
			fmt.Println("no recorded runs yet (~/.goaf/history)")
			return
		}
		fmt.Printf("%-22s %-10s %4s %8s %7s  %s\n", "TIME", "MODE", "OK", "CHANGED", "FAILED", "ID")
		for _, r := range runs {
			fmt.Printf("%-22s %-10s %4d %8d %7d  %s\n", r.Time, r.Mode, r.Ok, r.Changed, r.Failed, r.ID)
		}
		return
	}

	p := tea.NewProgram(tui.New(*invPath), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
