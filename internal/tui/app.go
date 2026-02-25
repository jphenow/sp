package tui

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/store"
)

// binaryCheckInterval is how often the TUI checks if the sp binary has changed.
const binaryCheckInterval = 10 * time.Second

// pollInterval is how often the TUI re-fetches the sprite list from the daemon
// to pick up newly added/imported sprites and status changes.
const pollInterval = 3 * time.Second

// view represents which screen the TUI is showing.
type view int

const (
	viewDashboard view = iota
	viewDetail
)

// FilterOptions holds the TUI's initial filter configuration.
type FilterOptions struct {
	Tags       []string
	PathPrefix string
	NameFilter string
}

// Model is the top-level Bubbletea model for the sp TUI.
type Model struct {
	client  *daemon.Client
	sprites []*store.Sprite
	tags    map[string][]string // sprite name -> tags

	// View state
	currentView view
	width       int
	height      int

	// Dashboard state
	cursor      int
	filterInput textinput.Model
	filtering   bool
	filterOpts  FilterOptions

	// Detail state
	selectedSprite *store.Sprite
	selectedTags   []string

	// Binary change detection
	startBinaryHash string // hash at TUI startup
	binaryChanged   bool   // true if we detected a new binary

	// Error display
	err     error
	message string
}

// spriteListMsg is sent when the sprite list is refreshed.
type spriteListMsg struct {
	sprites []*store.Sprite
	tags    map[string][]string
}

// stateUpdateMsg is sent when the daemon broadcasts a state change.
type stateUpdateMsg struct {
	update daemon.StateUpdate
}

// errMsg wraps an error for the TUI.
type errMsg struct {
	err error
}

// msgMsg carries a status message to display briefly.
type msgMsg string

// tickMsg is sent periodically to trigger a sprite list refresh.
type tickMsg time.Time

// binaryCheckMsg is sent periodically to check if the sp binary has changed.
type binaryCheckMsg time.Time

// binaryChangedMsg indicates the on-disk binary differs from what's running.
type binaryChangedMsg struct{}

// NewModel creates a new TUI model connected to the daemon.
func NewModel(client *daemon.Client, opts FilterOptions) Model {
	ti := textinput.New()
	ti.Placeholder = "filter by name, tag, or path..."
	ti.CharLimit = 100
	ti.Width = 40

	// Pre-populate filter if provided
	if opts.NameFilter != "" {
		ti.SetValue(opts.NameFilter)
	}

	hash, _ := daemon.BinaryHash()

	return Model{
		client:          client,
		filterInput:     ti,
		filterOpts:      opts,
		tags:            make(map[string][]string),
		startBinaryHash: hash,
	}
}

// Init returns the initial command to fetch sprites and start the periodic poll ticker.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchSprites,
		tea.SetWindowTitle("sp"),
		tickCmd(),
		binaryCheckCmd(),
	)
}

// binaryCheckCmd returns a tea.Cmd that sends a binaryCheckMsg after the check interval.
func binaryCheckCmd() tea.Cmd {
	return tea.Tick(binaryCheckInterval, func(t time.Time) tea.Msg {
		return binaryCheckMsg(t)
	})
}

// tickCmd returns a tea.Cmd that sends a tickMsg after pollInterval.
func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Update handles messages and user input.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case spriteListMsg:
		m.sprites = msg.sprites
		m.tags = msg.tags
		if m.cursor >= len(m.sprites) {
			m.cursor = max(0, len(m.sprites)-1)
		}
		return m, nil

	case tickMsg:
		// Periodic poll: re-fetch the sprite list and schedule the next tick.
		// This ensures new imports and status changes always appear.
		return m, tea.Batch(m.fetchSprites, tickCmd())

	case binaryCheckMsg:
		return m, tea.Batch(m.checkBinaryChanged, binaryCheckCmd())

	case binaryChangedMsg:
		m.binaryChanged = true
		// Auto re-exec: the TUI replaces itself with the new binary
		return m, m.reExec

	case stateUpdateMsg:
		// Refresh sprite list on any state change
		return m, m.fetchSprites

	case errMsg:
		m.err = msg.err
		return m, nil

	case msgMsg:
		m.message = string(msg)
		return m, nil
	}

	// Update filter input if filtering
	if m.filtering {
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

	return m, nil
}

// handleKey processes keyboard input based on current view.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys
	switch msg.String() {
	case "ctrl+c", "q":
		if m.filtering {
			m.filtering = false
			m.filterInput.Blur()
			return m, nil
		}
		if m.currentView == viewDetail {
			m.currentView = viewDashboard
			return m, nil
		}
		return m, tea.Quit
	}

	if m.filtering {
		return m.handleFilterKey(msg)
	}

	switch m.currentView {
	case viewDashboard:
		return m.handleDashboardKey(msg)
	case viewDetail:
		return m.handleDetailKey(msg)
	}

	return m, nil
}

// handleDashboardKey processes keys in the main list view.
func (m Model) handleDashboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.sprites)-1 {
			m.cursor++
		}
	case "enter":
		if len(m.sprites) > 0 && m.cursor < len(m.sprites) {
			m.selectedSprite = m.sprites[m.cursor]
			m.selectedTags = m.tags[m.selectedSprite.Name]
			m.currentView = viewDetail
		}
	case "f":
		m.filtering = true
		m.filterInput.Focus()
		return m, textinput.Blink
	case "r":
		return m, m.fetchSprites
	case "t":
		// Tag the selected sprite
		if len(m.sprites) > 0 && m.cursor < len(m.sprites) {
			return m, m.promptTag(m.sprites[m.cursor].Name)
		}
	}
	return m, nil
}

// handleDetailKey processes keys in the sprite detail view.
func (m Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.currentView = viewDashboard
	case "r":
		return m, m.fetchSprites
	}
	return m, nil
}

// handleFilterKey processes keys when the filter input is active.
func (m Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.filtering = false
		m.filterInput.Blur()
		m.filterOpts.NameFilter = m.filterInput.Value()
		return m, m.fetchSprites
	case "esc":
		m.filtering = false
		m.filterInput.Blur()
		m.filterInput.SetValue("")
		m.filterOpts.NameFilter = ""
		return m, m.fetchSprites
	}

	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	return m, cmd
}

// View renders the current state of the TUI.
func (m Model) View() string {
	switch m.currentView {
	case viewDetail:
		return m.viewDetail()
	default:
		return m.viewDashboard()
	}
}

// viewDashboard renders the main sprite list view.
func (m Model) viewDashboard() string {
	var b strings.Builder

	// Header
	running, warm, cold := 0, 0, 0
	for _, s := range m.sprites {
		switch s.Status {
		case "running":
			running++
		case "warm":
			warm++
		case "cold":
			cold++
		}
	}

	header := fmt.Sprintf("sp — %d sprites (%d running, %d warm, %d cold)",
		len(m.sprites), running, warm, cold)
	if len(m.filterOpts.Tags) > 0 {
		header += fmt.Sprintf("  [filter: %s]", strings.Join(m.filterOpts.Tags, ", "))
	}
	if m.filterOpts.PathPrefix != "" {
		header += fmt.Sprintf("  [prefix: %s]", m.filterOpts.PathPrefix)
	}
	b.WriteString(HeaderStyle.Render(header))
	b.WriteString("\n")

	// Filter input
	if m.filtering {
		b.WriteString(m.filterInput.View())
		b.WriteString("\n\n")
	}

	// Table header
	headerRow := fmt.Sprintf("  %-35s %-10s %-12s %-40s %s",
		"NAME", "STATUS", "SYNC", "LOCAL PATH", "TAGS")
	b.WriteString(NormalRowStyle.Render(headerRow))
	b.WriteString("\n")

	// Sprite rows
	if len(m.sprites) == 0 {
		b.WriteString(NormalRowStyle.Render("\n  No sprites found. Use 'sp import' to add existing sprites."))
	}

	for i, s := range m.sprites {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}

		localPath := s.LocalPath
		if len(localPath) > 38 {
			localPath = "…" + localPath[len(localPath)-37:]
		}

		tagList := ""
		if tags, ok := m.tags[s.Name]; ok && len(tags) > 0 {
			tagList = TagStyle.Render(strings.Join(tags, ", "))
		}

		name := s.Name
		if len(name) > 33 {
			name = name[:32] + "…"
		}

		row := fmt.Sprintf("%s%-35s %-10s %-12s %-40s %s",
			cursor, name,
			StatusStyle(s.Status),
			SyncStatusStyle(s.SyncStatus),
			localPath, tagList)

		if i == m.cursor {
			b.WriteString(SelectedRowStyle.Render(row))
		} else {
			b.WriteString(NormalRowStyle.Render(row))
		}
		b.WriteString("\n")
	}

	// Error/message display
	if m.err != nil {
		b.WriteString("\n")
		b.WriteString(ErrorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
	}
	if m.message != "" {
		b.WriteString("\n")
		b.WriteString(m.message)
	}

	// Help bar
	b.WriteString("\n")
	help := "[↑↓/jk] navigate  [enter] details  [f] filter  [t] tag  [r] refresh  [q] quit"
	b.WriteString(HelpStyle.Render(help))

	return b.String()
}

// viewDetail renders the detail view for a single sprite.
func (m Model) viewDetail() string {
	s := m.selectedSprite
	if s == nil {
		return "No sprite selected"
	}

	var b strings.Builder

	// Title
	title := fmt.Sprintf("%s    [%s] [%s]",
		s.Name, StatusStyle(s.Status), SyncStatusStyle(s.SyncStatus))
	b.WriteString(HeaderStyle.Render(title))
	b.WriteString("\n\n")

	// Details
	details := []struct {
		label string
		value string
	}{
		{"Sprite ID", s.SpriteID},
		{"URL", s.URL},
		{"Local", s.LocalPath},
		{"Remote", s.RemotePath},
		{"Repo", s.Repo},
		{"Org", s.Org},
		{"Status", StatusStyle(s.Status)},
		{"Sync Status", SyncStatusStyle(s.SyncStatus)},
		{"Created", s.CreatedAt.Format("2006-01-02 15:04:05")},
		{"Last Seen", s.LastSeen.Format("2006-01-02 15:04:05")},
	}

	for _, d := range details {
		if d.value == "" || d.value == "0001-01-01 00:00:00" {
			d.value = "-"
		}
		line := DetailLabelStyle.Render(d.label+":") + "  " + DetailValueStyle.Render(d.value)
		b.WriteString(line)
		b.WriteString("\n")
	}

	// Sync error
	if s.SyncError != "" {
		b.WriteString("\n")
		b.WriteString(DetailLabelStyle.Render("Sync Error:") + "  ")
		b.WriteString(ErrorStyle.Render(s.SyncError))
		b.WriteString("\n")
	}

	// Tags
	if len(m.selectedTags) > 0 {
		b.WriteString("\n")
		b.WriteString(DetailLabelStyle.Render("Tags:") + "  ")
		b.WriteString(TagStyle.Render(strings.Join(m.selectedTags, ", ")))
		b.WriteString("\n")
	}

	// Help
	b.WriteString("\n")
	help := "[esc] back  [r] refresh  [q] quit"
	b.WriteString(HelpStyle.Render(help))

	return b.String()
}

// fetchSprites is a tea.Cmd that queries the daemon for the current sprite list.
func (m Model) fetchSprites() tea.Msg {
	opts := store.ListOptions{
		Tags:       m.filterOpts.Tags,
		PathPrefix: m.filterOpts.PathPrefix,
		NameFilter: m.filterOpts.NameFilter,
	}

	sprites, err := m.client.ListSprites(opts)
	if err != nil {
		return errMsg{err: err}
	}

	// Fetch tags for each sprite
	tags := make(map[string][]string)
	for _, s := range sprites {
		t, err := m.client.GetTags(s.Name)
		if err == nil {
			tags[s.Name] = t
		}
	}

	return spriteListMsg{sprites: sprites, tags: tags}
}

// checkBinaryChanged is a tea.Cmd that compares the current binary hash to
// the one at startup. Sends binaryChangedMsg if they differ.
func (m Model) checkBinaryChanged() tea.Msg {
	if m.startBinaryHash == "" {
		return nil
	}
	current, err := daemon.BinaryHash()
	if err != nil {
		return nil
	}
	if current != m.startBinaryHash {
		return binaryChangedMsg{}
	}
	return nil
}

// reExec replaces the current TUI process with the new binary. It first tells
// the daemon to restart (so both pick up the new code), then execs itself.
func (m Model) reExec() tea.Msg {
	// Tell the daemon to restart too
	if m.client != nil {
		m.client.Restart()
	}

	// Re-exec ourselves with the same args
	exePath, err := os.Executable()
	if err != nil {
		return errMsg{err: fmt.Errorf("re-exec: %w", err)}
	}

	// syscall.Exec replaces the process — Bubbletea cleanup happens via the
	// alt screen restore that the terminal does on exec.
	execErr := syscall.Exec(exePath, os.Args, os.Environ())
	// If we get here, exec failed
	return errMsg{err: fmt.Errorf("re-exec failed: %w", execErr)}
}

// promptTag is a tea.Cmd that would open a tag input prompt.
// For now it just toggles a "work" tag for demonstration.
func (m Model) promptTag(name string) tea.Cmd {
	return func() tea.Msg {
		tags, err := m.client.GetTags(name)
		if err != nil {
			return errMsg{err: err}
		}

		// Toggle "work" tag
		hasWork := false
		for _, t := range tags {
			if t == "work" {
				hasWork = true
				break
			}
		}

		if hasWork {
			if err := m.client.UntagSprite(name, "work"); err != nil {
				return errMsg{err: err}
			}
			return msgMsg(fmt.Sprintf("Removed 'work' tag from %s", name))
		}
		if err := m.client.TagSprite(name, "work"); err != nil {
			return errMsg{err: err}
		}
		return msgMsg(fmt.Sprintf("Added 'work' tag to %s", name))
	}
}
