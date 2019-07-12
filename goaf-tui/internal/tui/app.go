package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"goaf-tui/internal/config"
	"goaf-tui/internal/inventory"
)

type panel int

const (
	panelInventory panel = iota
	panelTasks
	panelRunConfig
	panelMonitor
	panelCount
)

var panelTitles = [panelCount]string{
	"Inventory",
	"Tasks",
	"Run Config",
	"Monitor",
}

const (
	colorFocused = lipgloss.Color("69")
	colorMuted   = lipgloss.Color("240")
	colorBg      = lipgloss.Color("235")
	colorFg      = lipgloss.Color("252")
	colorGreen   = lipgloss.Color("76")
)

var (
	styleFocusedTitle = lipgloss.NewStyle().Bold(true).Foreground(colorFocused)
	styleMutedTitle   = lipgloss.NewStyle().Foreground(colorMuted)
	styleDim          = lipgloss.NewStyle().Foreground(colorMuted)
	styleKey          = lipgloss.NewStyle().Foreground(colorFocused)
	styleOn           = lipgloss.NewStyle().Foreground(colorGreen).Bold(true)
	styleCursor       = lipgloss.NewStyle().Background(colorFocused).Foreground(lipgloss.Color("0")).Bold(true)
	styleErr          = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleStatusBar    = lipgloss.NewStyle().Background(colorBg).Foreground(colorFg)
	styleSearchMatch  = lipgloss.NewStyle().Background(lipgloss.Color("226")).Foreground(lipgloss.Color("0")).Bold(true)
	styleCancel       = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
)

// invItem is one row in the rendered inventory tree.
type invItem struct {
	label     string
	isGroup   bool
	indent    int
	selected  bool
	collapsed bool
}

type hostState int

const (
	hostIdle    hostState = iota
	hostRunning           // actively executing current task
	hostOK                // last result was ok
	hostChanged           // last result was changed/would_change
	hostFailed            // last result was failed
	hostSkipped           // last result was skipped
)

// hostRunState tracks per-host progress during a run.
type hostRunState struct {
	state   hostState
	task    string // current or last task name
	ok      int
	changed int
	failed  int
	skipped int
}

type rcSetting int

const (
	rcParallel rcSetting = iota
	rcDryRun
	rcReport
	rcReportPath
	rcBecome
	rcSettingCount
)

type taskMode int

const (
	taskAdhoc    taskMode = iota
	taskPlaybook
	taskModeCount
)

type pbEntry struct {
	name  string
	isDir bool
}

// Model is the root Bubble Tea model.
type Model struct {
	focus  panel
	width  int
	height int

	// Inventory panel
	invCursor  int
	invScroll  int // top visible index in the visible list
	invInnerH  int // visible height, set from View
	invItems   []invItem
	invPath    string
	invErr     string
	invLoading bool
	invInput   textinput.Model

	// Tasks panel
	tMode        taskMode
	tEditing     bool
	tInput       textinput.Model
	adhocCmd     string
	playbookPath string
	// Playbook browser
	pbBrowsing bool
	pbDir      string
	pbFiles    []pbEntry
	pbCursor   int
	pbScroll   int

	// Run Config panel
	rcCursor   int
	parallel   int
	dryRun     bool
	report     bool
	reportPath string
	rcEditing  bool
	rcInput    textinput.Model
	become     bool

	// Run state
	running     bool
	cancelling  bool
	runCmd      *exec.Cmd
	runLines    []string
	runErr      string
	eventCh     <-chan eventMsg
	goafBin     string
	monScroll   int // display-line offset from the bottom (0 = latest)
	monSplit    int // monitor height as % of usable terminal height (20–80)
	monInnerW   int // panel inner width, set on WindowSizeMsg and in View
	monLogH     int // visible log lines, set in View, used for jump-to-match
	saveMsg     string
	hostStatus  map[string]*hostRunState
	hostOrder   []string // insertion order for table
	currentTask string

	// Search
	searching     bool
	searchTi      textinput.Model
	searchQuery   string
	searchMatches []int // indices into current displayLines
	searchIdx     int   // current match position in searchMatches
}

// New creates the initial TUI model. If invPath is non-empty it is loaded as
// the starting inventory; otherwise the last saved inventory from config is used.
func New(invPath string) Model {
	cfg := config.Load()

	invTi := textinput.New()
	invTi.Placeholder = "/path/to/inventory.yml"
	invTi.CharLimit = 512
	invTi.Width = 40

	taskTi := textinput.New()
	taskTi.CharLimit = 512

	rcTi := textinput.New()
	rcTi.Placeholder = "/tmp/goaf-report.json"
	rcTi.CharLimit = 512

	searchTi := textinput.New()
	searchTi.Placeholder = "search…"
	searchTi.CharLimit = 100

	m := Model{
		focus:      panelInventory,
		parallel:   cfg.Parallel,
		reportPath: cfg.ReportPath,
		monSplit:   cfg.MonSplit,
		dryRun:     cfg.DryRun,
		become:     cfg.Become,
		report:     cfg.Report,
		invInput:   invTi,
		tInput:     taskTi,
		rcInput:    rcTi,
		searchTi:   searchTi,
	}
	m.goafBin, _ = findGoaf()

	// CLI flag overrides saved inventory
	if invPath == "" {
		invPath = cfg.LastInventory
	}
	if invPath != "" {
		m = m.loadInventory(invPath)
	}
	return m
}

// pbStartDir returns the directory to open when the file browser is launched.
func (m Model) pbStartDir() string {
	if m.playbookPath != "" {
		if d := filepath.Dir(m.playbookPath); d != "" && d != "." {
			return d
		}
	}
	if m.invPath != "" {
		return filepath.Dir(m.invPath)
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// scanPbDir reads a directory and returns sorted entries (dirs first, then .yml files).
func scanPbDir(dir string) ([]pbEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs, files []pbEntry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if !strings.HasPrefix(name, ".") {
				dirs = append(dirs, pbEntry{name: name + "/", isDir: true})
			}
		} else if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			files = append(files, pbEntry{name: name})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].name < dirs[j].name })
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	// prepend ".." unless already at filesystem root
	result := []pbEntry{{name: "../", isDir: true}}
	result = append(result, dirs...)
	result = append(result, files...)
	return result, nil
}

// openPbDir switches the browser to a new directory.
func (m Model) openPbDir(dir string) Model {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	files, err := scanPbDir(abs)
	if err != nil {
		return m
	}
	m.pbDir = abs
	m.pbFiles = files
	m.pbCursor = 0
	m.pbScroll = 0
	return m
}

// loadInventory reads and parses a YAML inventory file, updating model state.
func (m Model) loadInventory(path string) Model {
	inv, err := inventory.Load(path)
	if err != nil {
		m.invErr = err.Error()
		return m
	}
	m.invErr = ""
	m.invPath = path
	m.invCursor = 0
	m.invItems = treeToItems(inventory.BuildTree(inv))
	return m
}

func treeToItems(tree []inventory.Item) []invItem {
	items := make([]invItem, len(tree))
	for i, t := range tree {
		items[i] = invItem{label: t.Label, isGroup: t.IsGroup, indent: t.Indent}
	}
	return items
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.monInnerW = msg.Width - 2
		m.invInnerH = m.computeInvInnerH()
		return m, nil

	case eventMsg:
		m = m.applyEvent(msg)
		lines := formatEvent(msg)
		m.runLines = append(m.runLines, lines...)
		return m, waitForEvent(m.eventCh)

	case runDoneMsg:
		m.running = false
		if m.cancelling {
			m.runLines = append(m.runLines, "", styleCancel.Render("⚠ Run interrupted by user"))
			m.cancelling = false
		}
		m.runCmd = nil
		return m, nil

	case tea.KeyMsg:
		key := msg.String()

		// Search input active — route all keys there
		if m.searching {
			return m.updateSearchInput(msg)
		}

		// Other text inputs
		if m.invLoading {
			return m.updateInvInput(msg)
		}
		if m.tEditing {
			return m.updateTaskInput(msg)
		}
		if m.rcEditing {
			return m.updateRCInput(msg)
		}

		// Global keys
		switch key {
		case "ctrl+c", "q":
			m.saveConfig()
			return m, tea.Quit
		case "tab":
			m.focus = (m.focus + 1) % panelCount
			return m, nil
		case "shift+tab", "\x1b[Z":
			m.focus = (m.focus + panelCount - 1) % panelCount
			return m, nil
		case "esc":
			if m.pbBrowsing {
				m.pbBrowsing = false
				return m, nil
			}
			if m.searchQuery != "" {
				m.searchQuery = ""
				m.searchMatches = nil
				m.searchTi.SetValue("")
				return m, nil
			}
			m.focus = panelInventory
			return m, nil
		case "1":
			m.focus = panelInventory
			return m, nil
		case "2":
			m.focus = panelTasks
			return m, nil
		case "3":
			m.focus = panelRunConfig
			return m, nil
		case "4":
			m.focus = panelMonitor
			return m, nil
		case "ctrl+f", "/":
			m.searching = true
			m.searchTi.SetValue(m.searchQuery)
			m.searchTi.Focus()
			m.focus = panelMonitor
			return m, nil
		case "ctrl+x":
			if m.running && m.runCmd != nil && m.runCmd.Process != nil {
				_ = m.runCmd.Process.Signal(os.Interrupt)
				m.cancelling = true
			}
			return m, nil
		case "r":
			if !m.running {
				return m.startRunCmd()
			}
			return m, nil
		}

		// Per-panel keys
		switch m.focus {
		case panelInventory:
			m = m.handleInventoryKey(key)
		case panelTasks:
			m = m.handleTasksKey(key)
		case panelRunConfig:
			m = m.handleRunConfigKey(key)
		case panelMonitor:
			m = m.handleMonitorKey(key)
		}
	}
	return m, nil
}

// updateInvInput handles keypresses while the inventory path input is active.
func (m Model) updateInvInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.invLoading = false
		m.invInput.Blur()
		m.invInput.SetValue("")
		return m, nil
	case "enter":
		path := strings.TrimSpace(m.invInput.Value())
		m.invLoading = false
		m.invInput.Blur()
		m.invInput.SetValue("")
		if path != "" {
			m = m.loadInventory(path)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.invInput, cmd = m.invInput.Update(msg)
	return m, cmd
}

// updateTaskInput handles keypresses while the task text input is active.
func (m Model) updateTaskInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.tEditing = false
		m.tInput.Blur()
		// Restore previous confirmed value
		if m.tMode == taskAdhoc {
			m.tInput.SetValue(m.adhocCmd)
		} else {
			m.tInput.SetValue(m.playbookPath)
		}
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.tInput.Value())
		m.tEditing = false
		m.tInput.Blur()
		if m.tMode == taskAdhoc {
			m.adhocCmd = val
		} else {
			m.playbookPath = val
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.tInput, cmd = m.tInput.Update(msg)
	return m, cmd
}

// updateRCInput handles keypresses while the report path input is active.
func (m Model) updateRCInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.rcEditing = false
		m.rcInput.Blur()
		m.rcInput.SetValue(m.reportPath)
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.rcInput.Value())
		m.rcEditing = false
		m.rcInput.Blur()
		if val != "" {
			m.reportPath = val
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.rcInput, cmd = m.rcInput.Update(msg)
	return m, cmd
}

// updateSearchInput handles keypresses while the search bar is active.
func (m Model) updateSearchInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.saveConfig()
		return m, tea.Quit
	case "esc":
		m.searching = false
		m.searchTi.Blur()
		// Keep query for highlight but close the bar
		return m, nil
	case "enter":
		m.searchQuery = strings.TrimSpace(m.searchTi.Value())
		m.searching = false
		m.searchTi.Blur()
		m = m.recomputeMatches()
		if len(m.searchMatches) > 0 {
			m.searchIdx = 0
			m = m.jumpToMatch()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.searchTi, cmd = m.searchTi.Update(msg)
	m.searchQuery = m.searchTi.Value()
	m = m.recomputeMatches()
	return m, cmd
}

// ### Per-panel key handling ###

func (m Model) handleInventoryKey(key string) Model {
	vis := m.visibleInvItems()
	vn := len(vis)

	switch key {
	case "l":
		m.invLoading = true
		m.invErr = ""
		m.invInput.Focus()

	case "up", "k":
		if m.invCursor > 0 {
			m.invCursor--
			m = m.clampInvScroll()
		}

	case "down", "j":
		if vn > 0 && m.invCursor < vn-1 {
			m.invCursor++
			m = m.clampInvScroll()
		}

	case "enter":
		if vn == 0 {
			break
		}
		raw := vis[m.invCursor]
		if m.invItems[raw].isGroup {
			m.invItems[raw].collapsed = !m.invItems[raw].collapsed
			// Clamp cursor in case visible count shrunk
			newVis := m.visibleInvItems()
			if m.invCursor >= len(newVis) {
				m.invCursor = len(newVis) - 1
			}
			m = m.clampInvScroll()
		}

	case " ":
		if vn == 0 {
			break
		}
		raw := vis[m.invCursor]
		m.invItems[raw].selected = !m.invItems[raw].selected
		newState := m.invItems[raw].selected
		if m.invItems[raw].isGroup {
			for j := raw + 1; j < len(m.invItems); j++ {
				if m.invItems[j].indent <= m.invItems[raw].indent {
					break
				}
				m.invItems[j].selected = newState
			}
		}

	case "a":
		for i := range m.invItems {
			m.invItems[i].selected = true
		}
	case "A":
		for i := range m.invItems {
			m.invItems[i].selected = false
		}
	}
	return m
}

// computeInvInnerH derives the inventory panel's usable line count from current dimensions.
func (m Model) computeInvInnerH() int {
	if m.height < 10 {
		return 10
	}
	usable := m.height - 1
	monOuterH := usable * m.monSplit / 100
	if monOuterH < 4 {
		monOuterH = 4
	}
	topOuterH := usable - monOuterH
	if topOuterH < 6 {
		topOuterH = 6
	}
	// topInnerH = topOuterH - 2 borders; then -1 for path label
	return topOuterH - 2 - 1
}

// visibleInvItems returns raw indices of items not hidden by a collapsed parent.
func (m Model) visibleInvItems() []int {
	var visible []int
	skipAboveIndent := -1
	for i, item := range m.invItems {
		if skipAboveIndent >= 0 {
			if item.indent > skipAboveIndent {
				continue
			}
			skipAboveIndent = -1
		}
		visible = append(visible, i)
		if item.isGroup && item.collapsed {
			skipAboveIndent = item.indent
		}
	}
	return visible
}

// clampInvScroll adjusts invScroll so invCursor stays in the visible window.
func (m Model) clampInvScroll() Model {
	h := m.invInnerH - 1 // -1 for path label
	if h < 3 {
		h = 10
	}
	if m.invCursor < m.invScroll {
		m.invScroll = m.invCursor
	}
	if m.invCursor >= m.invScroll+h {
		m.invScroll = m.invCursor - h + 1
	}
	if m.invScroll < 0 {
		m.invScroll = 0
	}
	return m
}

func (m Model) handleTasksKey(key string) Model {
	// ### File browser sub-mode ###
	if m.pbBrowsing {
		switch key {
		case "up", "k":
			if m.pbCursor > 0 {
				m.pbCursor--
				m = m.clampPbScroll()
			}
		case "down", "j":
			if m.pbCursor < len(m.pbFiles)-1 {
				m.pbCursor++
				m = m.clampPbScroll()
			}
		case "enter":
			if len(m.pbFiles) == 0 {
				break
			}
			entry := m.pbFiles[m.pbCursor]
			if entry.isDir {
				target := filepath.Join(m.pbDir, entry.name)
				m = m.openPbDir(target)
			} else {
				m.playbookPath = filepath.Join(m.pbDir, entry.name)
				m.pbBrowsing = false
			}
		case "backspace", "h", "left":
			m = m.openPbDir(filepath.Join(m.pbDir, ".."))
		case "esc":
			m.pbBrowsing = false
		case "e":
			m.pbBrowsing = false
			m.tInput.Placeholder = "/path/to/playbook.yml"
			m.tInput.SetValue(m.playbookPath)
			m.tEditing = true
			m.tInput.Focus()
		}
		return m
	}

	// ### Normal mode ###
	switch key {
	case "up", "k":
		if m.tMode > 0 {
			m.tMode--
		}
	case "down", "j":
		if m.tMode < taskModeCount-1 {
			m.tMode++
		}
	case "enter":
		if m.tMode == taskAdhoc {
			m.tInput.Placeholder = "e.g.  uptime"
			m.tInput.SetValue(m.adhocCmd)
			m.tEditing = true
			m.tInput.Focus()
		} else {
			// Open file browser
			m = m.openPbDir(m.pbStartDir())
			m.pbBrowsing = true
		}
	case "e":
		// Always allow manual text entry
		if m.tMode == taskAdhoc {
			m.tInput.Placeholder = "e.g.  uptime"
			m.tInput.SetValue(m.adhocCmd)
		} else {
			m.tInput.Placeholder = "/path/to/playbook.yml"
			m.tInput.SetValue(m.playbookPath)
		}
		m.tEditing = true
		m.tInput.Focus()
	case "x":
		if m.tMode == taskAdhoc {
			m.adhocCmd = ""
		} else {
			m.playbookPath = ""
		}
		m.tInput.SetValue("")
	}
	return m
}

func (m Model) clampPbScroll() Model {
	h := 8 // approximate visible rows in browser
	if m.pbCursor < m.pbScroll {
		m.pbScroll = m.pbCursor
	}
	if m.pbCursor >= m.pbScroll+h {
		m.pbScroll = m.pbCursor - h + 1
	}
	if m.pbScroll < 0 {
		m.pbScroll = 0
	}
	return m
}

func (m Model) handleRunConfigKey(key string) Model {
	switch key {
	case "up", "k":
		cur := m.rcCursor - 1
		// skip rcReportPath if report is off
		if rcSetting(cur) == rcReportPath && !m.report {
			cur--
		}
		if cur >= 0 {
			m.rcCursor = cur
		}
	case "down", "j":
		cur := m.rcCursor + 1
		// skip rcReportPath if report is off
		if rcSetting(cur) == rcReportPath && !m.report {
			cur++
		}
		if cur < int(rcSettingCount) {
			m.rcCursor = cur
		}
	case " ", "enter":
		switch rcSetting(m.rcCursor) {
		case rcDryRun:
			m.dryRun = !m.dryRun
		case rcReport:
			m.report = !m.report
			if m.report {
				// jump cursor to path row so user can edit it immediately
				m.rcCursor = int(rcReportPath)
			} else if m.rcCursor == int(rcReportPath) {
				m.rcCursor = int(rcReport)
			}
		case rcReportPath:
			if m.report {
				m.rcInput.SetValue(m.reportPath)
				m.rcInput.Focus()
				m.rcEditing = true
			}
		case rcBecome:
			m.become = !m.become
		}
	case "right", "l", "+":
		if rcSetting(m.rcCursor) == rcParallel && m.parallel < 50 {
			m.parallel++
		}
	case "left", "h", "-":
		if rcSetting(m.rcCursor) == rcParallel && m.parallel > 1 {
			m.parallel--
		}
	}
	return m
}

// applyEvent updates the host status table from an incoming NDJSON event.
func (m Model) applyEvent(ev eventMsg) Model {
	str := func(k string) string { v, _ := ev.data[k].(string); return v }
	num := func(k string) int { v, _ := ev.data[k].(float64); return int(v) }

	switch ev.eventType {
	case "task_started":
		m.currentTask = str("task")
		// mark all known hosts as running for this task
		for _, h := range m.hostOrder {
			if s := m.hostStatus[h]; s != nil && s.state != hostFailed {
				s.state = hostRunning
				s.task = m.currentTask
			}
		}

	case "task_result":
		host, status := str("host"), str("status")
		m.ensureHost(host)
		s := m.hostStatus[host]
		s.task = m.currentTask
		switch status {
		case "ok":
			s.state = hostOK
			s.ok++
		case "changed", "would_change":
			s.state = hostChanged
			s.changed++
			s.ok++
		case "failed":
			s.state = hostFailed
			s.failed++
		case "skipped":
			s.state = hostSkipped
			s.skipped++
		}

	case "facts_gathered":
		host := str("host")
		m.ensureHost(host)
		m.hostStatus[host].state = hostOK
		m.hostStatus[host].task = "gather_facts"

	case "host_recap":
		host := str("host")
		m.ensureHost(host)
		s := m.hostStatus[host]
		s.ok = num("ok")
		s.changed = num("changed")
		s.failed = num("failed")
		s.skipped = num("skipped")
		if s.failed > 0 {
			s.state = hostFailed
		} else if s.changed > 0 {
			s.state = hostChanged
		} else {
			s.state = hostOK
		}
		s.task = "done"
	}
	return m
}

func (m *Model) ensureHost(host string) {
	if m.hostStatus == nil {
		m.hostStatus = make(map[string]*hostRunState)
	}
	if _, ok := m.hostStatus[host]; !ok {
		m.hostStatus[host] = &hostRunState{}
		m.hostOrder = append(m.hostOrder, host)
	}
}

func (m Model) handleMonitorKey(key string) Model {
	total := len(m.runLines)
	switch key {
	case "up", "k":
		m.monScroll++
	case "down", "j":
		if m.monScroll > 0 {
			m.monScroll--
		}
	case "g":
		m.monScroll = total
	case "G", "end":
		m.monScroll = 0
	case "n":
		if m.searchQuery != "" {
			m = m.recomputeMatches()
			if len(m.searchMatches) > 0 {
				m.searchIdx = (m.searchIdx + 1) % len(m.searchMatches)
				m = m.jumpToMatch()
			}
		}
	case "N":
		if m.searchQuery != "" {
			m = m.recomputeMatches()
			if len(m.searchMatches) > 0 {
				m.searchIdx = (m.searchIdx + len(m.searchMatches) - 1) % len(m.searchMatches)
				m = m.jumpToMatch()
			}
		}
	case "+", "=":
		if m.monSplit < 80 {
			m.monSplit += 5
			m.invInnerH = m.computeInvInnerH()
		}
	case "-":
		if m.monSplit > 20 {
			m.monSplit -= 5
			m.invInnerH = m.computeInvInnerH()
		}
	case "s":
		m.saveMsg = m.saveLog()
	}
	return m
}

// computeDisplayLines wraps all runLines into display-width chunks.
func (m Model) computeDisplayLines() []string {
	w := m.monInnerW
	if w < 20 {
		w = 80
	}
	var out []string
	for _, line := range m.runLines {
		out = append(out, wrapLine(line, w)...)
	}
	return out
}

// findSearchMatches returns indices of displayLines that contain query (case-insensitive).
func findSearchMatches(lines []string, query string) []int {
	if query == "" {
		return nil
	}
	q := strings.ToLower(query)
	var out []int
	for i, line := range lines {
		if strings.Contains(strings.ToLower(line), q) {
			out = append(out, i)
		}
	}
	return out
}

// recomputeMatches refreshes searchMatches from current display lines and query.
func (m Model) recomputeMatches() Model {
	dl := m.computeDisplayLines()
	m.searchMatches = findSearchMatches(dl, m.searchQuery)
	if m.searchIdx >= len(m.searchMatches) {
		m.searchIdx = 0
	}
	return m
}

// jumpToMatch sets monScroll so the current match is visible near the centre.
func (m Model) jumpToMatch() Model {
	if len(m.searchMatches) == 0 {
		return m
	}
	matchLine := m.searchMatches[m.searchIdx]
	dl := m.computeDisplayLines()
	total := len(dl)
	logH := m.monLogH
	if logH < 4 {
		logH = 10
	}
	// Put match in the upper quarter of the visible area
	targetEnd := matchLine + logH*3/4
	if targetEnd > total {
		targetEnd = total
	}
	scroll := total - targetEnd
	if scroll < 0 {
		scroll = 0
	}
	m.monScroll = scroll
	return m
}

// saveConfig persists current settings to ~/.config/goaf-tui/config.yml.
func (m Model) saveConfig() {
	_ = config.Save(config.Config{
		LastInventory: m.invPath,
		Parallel:      m.parallel,
		MonSplit:      m.monSplit,
		DryRun:        m.dryRun,
		Become:        m.become,
		Report:        m.report,
		ReportPath:    m.reportPath,
	})
}

// wrapLine splits a plain-text line into chunks of at most maxW runes so that
// every chunk fits within the panel without truncation.
func wrapLine(line string, maxW int) []string {
	if maxW <= 0 {
		return []string{line}
	}
	runes := []rune(line)
	if len(runes) <= maxW {
		return []string{line}
	}
	var out []string
	for len(runes) > 0 {
		n := maxW
		if n > len(runes) {
			n = len(runes)
		}
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}

// saveLog writes m.runLines (plain text) to a timestamped file.
// Returns a status message for display.
func (m Model) saveLog() string {
	if len(m.runLines) == 0 {
		return "nothing to save"
	}
	path := fmt.Sprintf("/tmp/goaf-%s.log", time.Now().Format("20060102-150405"))
	content := strings.Join(m.runLines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Sprintf("save failed: %v", err)
	}
	return fmt.Sprintf("saved → %s", path)
}

// startRunCmd validates current settings, builds args, launches goaf subprocess.
func (m Model) startRunCmd() (Model, tea.Cmd) {
	if m.goafBin == "" {
		bin, err := findGoaf()
		if err != nil {
			m.runErr = err.Error()
			m.focus = panelMonitor
			return m, nil
		}
		m.goafBin = bin
	}

	args, err := m.buildArgs()
	if err != nil {
		m.runErr = err.Error()
		m.focus = panelMonitor
		return m, nil
	}

	cmd, ch, err := startRun(m.goafBin, args)
	if err != nil {
		m.runErr = err.Error()
		m.focus = panelMonitor
		return m, nil
	}

	m.running = true
	m.cancelling = false
	m.runCmd = cmd
	m.runErr = ""
	m.runLines = []string{fmt.Sprintf("$ goaf %s", strings.Join(args, " "))}
	m.eventCh = ch
	m.monScroll = 0
	m.searchQuery = ""
	m.searchMatches = nil
	m.searchIdx = 0
	m.hostStatus = make(map[string]*hostRunState)
	m.hostOrder = nil
	m.currentTask = ""
	m.focus = panelMonitor
	m.saveConfig()
	return m, waitForEvent(ch)
}

// buildArgs builds the goaf CLI arguments from the current model state.
func (m Model) buildArgs() ([]string, error) {
	if m.invPath == "" {
		return nil, fmt.Errorf("no inventory loaded — press l in Inventory panel")
	}

	args := []string{"-json"}
	if m.dryRun {
		args = append(args, "-check")
	}
	if m.become {
		args = append(args, "-become")
	}
	args = append(args, "-i", m.invPath)
	args = append(args, "-p", fmt.Sprintf("%d", m.parallel))
	if m.report && m.reportPath != "" {
		args = append(args, "-report", m.reportPath)
	}

	switch m.tMode {
	case taskAdhoc:
		target := m.selectedTarget()
		if target == "" {
			return nil, fmt.Errorf("no host or group selected — use Space in Inventory panel")
		}
		if m.adhocCmd == "" {
			return nil, fmt.Errorf("no command set — press Enter in Tasks panel to type one")
		}
		args = append(args, "-t", target, "command", m.adhocCmd)

	case taskPlaybook:
		if m.playbookPath == "" {
			return nil, fmt.Errorf("no playbook path set — press Enter in Tasks panel to type one")
		}
		args = append(args, "run", m.playbookPath)
	}

	return args, nil
}

// selectedTarget returns all selected groups (comma-separated), falling back
// to selected individual hosts if no groups are selected.
func (m Model) selectedTarget() string {
	var groups, hosts []string
	for _, item := range m.invItems {
		if !item.selected {
			continue
		}
		if item.isGroup {
			groups = append(groups, item.label)
		} else {
			hosts = append(hosts, item.label)
		}
	}
	if len(groups) > 0 {
		return strings.Join(groups, ",")
	}
	return strings.Join(hosts, ",")
}

// ### View ###

func (m Model) View() string {
	if m.width < 40 || m.height < 10 {
		return "Terminal too small (minimum 40×10)\n"
	}

	statusH := 1
	usable := m.height - statusH
	monOuterH := usable * m.monSplit / 100
	if monOuterH < 4 {
		monOuterH = 4
	}
	topOuterH := usable - monOuterH
	if topOuterH < 6 {
		topOuterH = 6
		monOuterH = usable - topOuterH
	}
	if monOuterH < 4 {
		monOuterH = 4
	}
	topInnerH := topOuterH - 2
	monInnerH := monOuterH - 2

	invOuterW := m.width * 27 / 100
	rcOuterW := m.width * 27 / 100
	taskOuterW := m.width - invOuterW - rcOuterW

	topRow := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderPanel(panelInventory, invOuterW-2, topInnerH, m.contentInventory(invOuterW-4, topInnerH-1)),
		m.renderPanel(panelTasks, taskOuterW-2, topInnerH, m.contentTasks(taskOuterW-4, topInnerH-1)),
		m.renderPanel(panelRunConfig, rcOuterW-2, topInnerH, m.contentRunConfig(topInnerH-1)),
	)
	m.monInnerW = m.width - 2 // panel innerW passed to renderPanel
	monRow := m.renderPanel(panelMonitor, m.width-2, monInnerH, m.contentMonitor(monInnerH-1))

	return lipgloss.JoinVertical(lipgloss.Left,
		topRow,
		monRow,
		m.renderStatusBar(),
	)
}

func (m Model) renderPanel(p panel, innerW, innerH int, content string) string {
	focused := m.focus == p
	borderColor := colorMuted
	titleStyle := styleMutedTitle
	if focused {
		borderColor = colorFocused
		titleStyle = styleFocusedTitle
	}

	title := titleStyle.Render(panelTitles[p])
	body := lipgloss.NewStyle().
		Width(innerW).
		Height(innerH - 1).
		Render(content)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(innerW).
		Render(title + "\n" + body)
}

func (m Model) renderStatusBar() string {
	var focusLabel string
	if m.running {
		focusLabel = fmt.Sprintf(" ● %s  %s", panelTitles[m.focus],
			lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render("RUNNING"))
	} else if m.saveMsg != "" {
		focusLabel = fmt.Sprintf(" ● %s  %s", panelTitles[m.focus],
			styleOn.Render(m.saveMsg))
	} else {
		focusLabel = fmt.Sprintf(" ● %s", panelTitles[m.focus])
	}

	var hints []string
	if m.searching {
		hints = []string{
			styleKey.Render("Enter") + " confirm",
			styleKey.Render("Esc") + " close",
			styleKey.Render("n/N") + " next/prev",
		}
	} else if m.invLoading || m.tEditing || m.rcEditing {
		hints = []string{
			styleKey.Render("Enter") + " confirm",
			styleKey.Render("Esc") + " cancel",
		}
	} else if m.cancelling {
		hints = []string{styleCancel.Render("interrupting run…")}
	} else if m.running {
		hints = []string{
			styleKey.Render("Tab") + " panel",
			styleKey.Render("↑↓") + " scroll",
			styleKey.Render("Ctrl+X") + " interrupt",
			styleKey.Render("q") + " quit",
		}
	} else {
		switch m.focus {
		case panelInventory:
			hints = []string{
				styleKey.Render("↑↓") + " nav",
				styleKey.Render("Enter") + " expand/collapse",
				styleKey.Render("Space") + " select",
				styleKey.Render("a/A") + " all/none",
				styleKey.Render("l") + " load",
				styleKey.Render("r") + " run",
				styleKey.Render("1-4") + " jump",
				styleKey.Render("q") + " quit",
			}
		case panelTasks:
			if m.pbBrowsing {
				hints = []string{
					styleKey.Render("↑↓") + " nav",
					styleKey.Render("Enter") + " select/open",
					styleKey.Render("h") + " up",
					styleKey.Render("e") + " type path",
					styleKey.Render("Esc") + " close",
				}
			} else {
				hints = []string{
					styleKey.Render("↑↓") + " mode",
					styleKey.Render("Enter") + " browse",
					styleKey.Render("e") + " type",
					styleKey.Render("x") + " clear",
					styleKey.Render("r") + " run",
					styleKey.Render("1-4") + " jump",
					styleKey.Render("q") + " quit",
				}
			}
		case panelRunConfig:
			hints = []string{
				styleKey.Render("↑↓") + " nav",
				styleKey.Render("←→") + " parallel",
				styleKey.Render("Space") + " toggle",
				styleKey.Render("r") + " run",
				styleKey.Render("1-4") + " jump",
				styleKey.Render("q") + " quit",
			}
		case panelMonitor:
			split := fmt.Sprintf("%d%%", m.monSplit)
			hints = []string{
				styleKey.Render("↑↓") + " scroll",
				styleKey.Render("g/G") + " top/end",
				styleKey.Render("/") + " search",
				styleKey.Render("+/-") + " resize(" + split + ")",
				styleKey.Render("s") + " save",
				styleKey.Render("r") + " run",
				styleKey.Render("1-4") + " jump",
				styleKey.Render("q") + " quit",
			}
		}
	}

	right := styleDim.Render(strings.Join(hints, "  ") + " ")
	gap := m.width - lipgloss.Width(focusLabel) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return styleStatusBar.Width(m.width).Render(focusLabel + strings.Repeat(" ", gap) + right)
}

// ### Panel content ###

func (m Model) contentInventory(innerW, maxLines int) string {
	var lines []string

	// Path input mode
	if m.invLoading {
		m.invInput.Width = innerW - 4
		lines = append(lines,
			"Load inventory file:",
			m.invInput.View(),
		)
		if m.invErr != "" {
			lines = append(lines, styleErr.Render("✗ "+m.invErr))
		}
		return strings.Join(lines, "\n")
	}

	// No inventory loaded
	if len(m.invItems) == 0 {
		lines = []string{
			styleDim.Render("No inventory loaded."),
			"",
			styleKey.Render("l") + "  load inventory file",
		}
		if m.invErr != "" {
			lines = append(lines, "", styleErr.Render("✗ "+m.invErr))
		}
		return strings.Join(lines, "\n")
	}

	// Path label (1 line)
	pathLabel := styleDim.Render("▸ " + truncate(m.invPath, innerW-2))
	lines = append(lines, pathLabel)
	maxLines-- // path label consumed 1

	vis := m.visibleInvItems()
	total := len(vis)

	// Clamp scroll
	scroll := m.invScroll
	if scroll > total-maxLines {
		scroll = total - maxLines
	}
	if scroll < 0 {
		scroll = 0
	}

	end := scroll + maxLines
	if end > total {
		end = total
	}

	for displayIdx, raw := range vis[scroll:end] {
		item := m.invItems[raw]
		visIdx := scroll + displayIdx

		indent := strings.Repeat("  ", item.indent)
		check := "[ ]"
		if item.selected {
			check = styleOn.Render("[✓]")
		}

		var label string
		if item.isGroup {
			icon := "▾"
			if item.collapsed {
				icon = "▸"
			}
			label = lipgloss.NewStyle().Bold(true).Foreground(colorFocused).Render(icon) +
				" " + lipgloss.NewStyle().Bold(true).Render(item.label)
		} else {
			label = item.label
		}

		line := fmt.Sprintf("%s%s %s", indent, check, label)
		if visIdx == m.invCursor && m.focus == panelInventory {
			line = styleCursor.Render("▶") + " " + strings.TrimLeft(
				fmt.Sprintf("%s%s %s", indent, check, label), " ")
		}
		lines = append(lines, line)
	}

	// Scroll indicator
	if total > maxLines {
		below := total - end
		indicator := ""
		if scroll > 0 && below > 0 {
			indicator = fmt.Sprintf("↑%d  ↓%d", scroll, below)
		} else if scroll > 0 {
			indicator = fmt.Sprintf("↑%d more", scroll)
		} else {
			indicator = fmt.Sprintf("↓%d more", below)
		}
		lines = append(lines, styleDim.Render("  "+indicator))
	}

	return strings.Join(lines, "\n")
}

func (m Model) contentTasks(innerW, maxLines int) string {
	focused := m.focus == panelTasks

	adhocPfx := "  "
	playbookPfx := "  "
	if focused && !m.pbBrowsing {
		if m.tMode == taskAdhoc {
			adhocPfx = styleCursor.Render("▶") + " "
		} else {
			playbookPfx = styleCursor.Render("▶") + " "
		}
	}

	m.tInput.Width = innerW - 4

	var lines []string

	// ### Ad-hoc section ###
	lines = append(lines, adhocPfx+lipgloss.NewStyle().Bold(true).Render("Ad-hoc command:"))
	if m.tEditing && m.tMode == taskAdhoc {
		lines = append(lines, "  "+m.tInput.View())
	} else if m.adhocCmd != "" {
		lines = append(lines, "  "+styleKey.Render("$ ")+m.adhocCmd)
	} else {
		lines = append(lines, "  "+styleDim.Render("(empty — Enter to type)"))
	}

	lines = append(lines, "")

	// ### Playbook section ###
	lines = append(lines, playbookPfx+lipgloss.NewStyle().Bold(true).Render("Playbook:"))
	if m.tEditing && m.tMode == taskPlaybook {
		lines = append(lines, "  "+m.tInput.View())
	} else if m.playbookPath != "" {
		lines = append(lines, "  "+styleKey.Render("▸ ")+truncate(m.playbookPath, innerW-6))
	} else {
		lines = append(lines, "  "+styleDim.Render("(none — Enter to browse)"))
	}

	// ### File browser (when active) ###
	if m.pbBrowsing {
		lines = append(lines, "")
		dirLabel := truncate(m.pbDir, innerW-2)
		lines = append(lines, styleDim.Render("  "+dirLabel))

		// available rows for file list
		used := len(lines) + 1 // +1 for bottom hint
		avail := maxLines - used
		if avail < 3 {
			avail = 3
		}

		total := len(m.pbFiles)
		scroll := m.pbScroll
		end := scroll + avail
		if end > total {
			end = total
		}

		for i, entry := range m.pbFiles[scroll:end] {
			visIdx := scroll + i
			icon := "  "
			var name string
			if entry.isDir {
				name = styleDim.Render(entry.name)
			} else {
				name = entry.name
			}
			row := icon + name
			if visIdx == m.pbCursor && focused {
				row = styleCursor.Render("▶") + " " + entry.name
			}
			lines = append(lines, "  "+row)
		}

		if total > avail {
			below := total - end
			ind := ""
			if scroll > 0 && below > 0 {
				ind = fmt.Sprintf("↑%d  ↓%d", scroll, below)
			} else if scroll > 0 {
				ind = fmt.Sprintf("↑%d more", scroll)
			} else {
				ind = fmt.Sprintf("↓%d more", below)
			}
			lines = append(lines, styleDim.Render("  "+ind))
		}

		lines = append(lines, styleDim.Render("  Enter=select  h=up  e=type  Esc=close"))
		return strings.Join(lines, "\n")
	}

	lines = append(lines, "")

	// ### Hints ###
	if m.tEditing {
		lines = append(lines, styleDim.Render("Enter  confirm   Esc  cancel"))
	} else if focused {
		lines = append(lines,
			styleKey.Render("↑↓")+" mode  "+
				styleKey.Render("Enter")+" browse  "+
				styleKey.Render("e")+" type  "+
				styleKey.Render("x")+" clear",
		)
	}

	return strings.Join(lines, "\n")
}

func (m Model) contentRunConfig(maxLines int) string {
	focused := m.focus == panelRunConfig

	type row struct {
		setting rcSetting
		label   string
	}
	rows := []row{
		{rcParallel, "Parallel"},
		{rcDryRun, "Dry-run "},
		{rcReport, "Report  "},
		{rcReportPath, "  Path  "},
		{rcBecome, "Become  "},
	}

	var lines []string
	for _, r := range rows {
		i := int(r.setting)

		// Report path row: only show when report is on
		if r.setting == rcReportPath && !m.report {
			continue
		}

		var val string
		switch r.setting {
		case rcParallel:
			val = styleKey.Render(fmt.Sprintf("%d", m.parallel))
		case rcReportPath:
			if m.rcEditing && m.rcCursor == i {
				val = m.rcInput.View()
			} else {
				val = styleDim.Render(truncate(m.reportPath, 16))
			}
		default:
			on := (r.setting == rcDryRun && m.dryRun) ||
				(r.setting == rcReport && m.report) ||
				(r.setting == rcBecome && m.become)
			if on {
				val = styleOn.Render("on")
			} else {
				val = styleDim.Render("off")
			}
		}

		line := fmt.Sprintf("%s  %s", r.label, val)
		if focused && i == m.rcCursor {
			line = styleCursor.Render("▶") + " " + line
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}

	lines = append(lines, "")
	if focused && !m.rcEditing {
		switch rcSetting(m.rcCursor) {
		case rcParallel:
			lines = append(lines, styleDim.Render("← → or +/- to adjust"))
		case rcReportPath:
			lines = append(lines, styleDim.Render("Enter to edit path"))
		default:
			lines = append(lines, styleDim.Render("Space to toggle on/off"))
		}
	} else if !focused {
		lines = append(lines, styleDim.Render("↑↓ Space to edit"))
	}
	return strings.Join(lines, "\n")
}

func (m Model) contentMonitor(maxLines int) string {
	if len(m.runLines) == 0 && len(m.hostOrder) == 0 {
		lines := []string{styleDim.Render("No active run.")}
		if m.runErr != "" {
			lines = append(lines, "", styleErr.Render("✗ "+m.runErr))
		} else {
			lines = append(lines,
				"",
				styleDim.Render("Configure inventory + task, then press  r  to run."),
			)
		}
		return strings.Join(lines, "\n")
	}

	// ### Host status table ###
	tableLines := m.renderHostTable()
	tableH := len(tableLines)
	if tableH > maxLines/2 {
		tableH = maxLines / 2
	}
	tableLines = tableLines[:tableH]

	// ### Search bar ###
	searchBarH := 0
	var searchBarLine string
	if m.searching {
		searchBarH = 1
		m.searchTi.Width = m.monInnerW - 12
		matchInfo := ""
		if m.searchQuery != "" {
			n := len(m.searchMatches)
			if n == 0 {
				matchInfo = styleErr.Render("  no matches")
			} else {
				matchInfo = styleDim.Render(fmt.Sprintf("  %d/%d", m.searchIdx+1, n))
			}
		}
		searchBarLine = styleKey.Render("  ⌕ ") + m.searchTi.View() + matchInfo
	} else if m.searchQuery != "" {
		searchBarH = 1
		n := len(m.searchMatches)
		searchBarLine = styleDim.Render(fmt.Sprintf("  ⌕ %q  %d matches   n/N nav   Esc clear", m.searchQuery, n))
	}

	// ### Log section ###
	logH := maxLines - tableH - searchBarH - 1 // -1 for indicator
	if logH < 2 {
		logH = 2
	}
	m.monLogH = logH // used by jumpToMatch

	displayLines := m.computeDisplayLines()
	// Also refresh match indices (terminal may have resized since last compute)
	localMatches := findSearchMatches(displayLines, m.searchQuery)

	total := len(displayLines)
	maxScroll := total - logH
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.monScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}

	end := total - scroll
	start := end - logH
	if start < 0 {
		start = 0
	}

	var logLines []string
	for _, line := range displayLines[start:end] {
		if m.searchQuery != "" && strings.Contains(strings.ToLower(line), strings.ToLower(m.searchQuery)) {
			logLines = append(logLines, styleSearchMatch.Render(line))
		} else if m.searchQuery != "" {
			logLines = append(logLines, styleDim.Render(line))
		} else {
			logLines = append(logLines, colorLine(line))
		}
	}
	for len(logLines) < logH {
		logLines = append(logLines, "")
	}

	// Scroll / status indicator
	var vpart string
	switch {
	case m.cancelling:
		vpart = styleCancel.Render("⚠ interrupting…")
	case m.running && scroll == 0:
		vpart = "● running…   ↑/k scroll"
	case m.running:
		vpart = fmt.Sprintf("● running — %d–%d/%d   ↓/j follow", start+1, end, total)
	case m.searchQuery != "":
		vpart = fmt.Sprintf("%d/%d display lines   %d matches   n/N nav", end-start, total, len(localMatches))
	case scroll > 0:
		vpart = fmt.Sprintf("%d–%d/%d   ↓/j newer   G end", start+1, end, total)
	default:
		vpart = fmt.Sprintf("%d display lines   /  search   r new run", total)
	}
	indicator := styleDim.Render("  " + vpart)

	var all []string
	all = append(all, tableLines...)
	if searchBarH > 0 {
		all = append(all, searchBarLine)
	}
	all = append(all, logLines...)
	all = append(all, indicator)
	return strings.Join(all, "\n")
}

// renderHostTable returns lines for the per-host status table.
func (m Model) renderHostTable() []string {
	if len(m.hostOrder) == 0 {
		return nil
	}

	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinChar := spinner[(len(m.runLines))%len(spinner)]

	styleRunning := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleChanged := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleOK := lipgloss.NewStyle().Foreground(colorGreen)
	styleFail := styleErr
	styleSkip := styleDim

	sep := styleDim.Render("  ─────────────────────────────────────────────")
	lines := []string{sep}

	for _, host := range m.hostOrder {
		s := m.hostStatus[host]
		if s == nil {
			continue
		}

		var stateIcon, stateStr string
		var stateStyle lipgloss.Style
		switch s.state {
		case hostRunning:
			stateIcon = spinChar
			stateStr = "running"
			stateStyle = styleRunning
		case hostOK:
			stateIcon = "✓"
			stateStr = "ok"
			stateStyle = styleOK
		case hostChanged:
			stateIcon = "~"
			stateStr = "changed"
			stateStyle = styleChanged
		case hostFailed:
			stateIcon = "✗"
			stateStr = "failed"
			stateStyle = styleFail
		case hostSkipped:
			stateIcon = "-"
			stateStr = "skipped"
			stateStyle = styleSkip
		default:
			stateIcon = "·"
			stateStr = "idle"
			stateStyle = styleDim
		}

		task := s.task
		if len(task) > 22 {
			task = task[:21] + "…"
		}

		counters := fmt.Sprintf("ok:%-2d chg:%-2d fail:%-2d", s.ok, s.changed, s.failed)
		hostCol := fmt.Sprintf("%-22s", host)
		taskCol := fmt.Sprintf("%-23s", task)

		line := fmt.Sprintf("  %s %s  %s  %s  %s",
			stateStyle.Render(stateIcon),
			styleDim.Render(hostCol),
			stateStyle.Bold(true).Render(fmt.Sprintf("%-7s", stateStr)),
			styleDim.Render(taskCol),
			styleDim.Render(counters),
		)
		lines = append(lines, line)
	}
	lines = append(lines, sep)
	return lines
}

// ### Helpers ###

func boolLabel(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max+1:]
}
