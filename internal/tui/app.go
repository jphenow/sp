package tui

import (
	"fmt"
	"os"
	"os/exec"
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
	viewTagInput
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

	// Tag input state
	tagInput    textinput.Model
	tagging     bool   // when true, the tag text input is active
	tagTarget   string // sprite name being tagged
	tagRemoving bool   // true if the 'T' (remove) variant was pressed

	// Delete confirmation state
	confirmDelete bool   // when true, waiting for y/n to confirm delete
	deleteName    string // name of sprite pending deletion

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

// consoleFinishedMsg is sent when an interactive console session ends and
// control returns to the TUI.
type consoleFinishedMsg struct {
	err error
}

// syncToggledMsg is sent after a sync start/stop operation completes.
type syncToggledMsg struct {
	name   string
	action string // "started" or "stopped"
	err    error
}

// deleteResultMsg is sent after a sprite delete operation completes.
type deleteResultMsg struct {
	name string
	err  error
}

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

	tagTi := textinput.New()
	tagTi.Placeholder = "tag name..."
	tagTi.CharLimit = 50
	tagTi.Width = 30

	return Model{
		client:          client,
		filterInput:     ti,
		tagInput:        tagTi,
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

	case consoleFinishedMsg:
		// Console session ended — refresh sprites to pick up any changes.
		if msg.err != nil {
			m.err = msg.err
		}
		return m, m.fetchSprites

	case syncToggledMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.message = fmt.Sprintf("Sync %s for %s", msg.action, msg.name)
		}
		return m, m.fetchSprites

	case deleteResultMsg:
		m.confirmDelete = false
		m.deleteName = ""
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.message = fmt.Sprintf("Deleted sprite %s", msg.name)
			// If we were viewing the deleted sprite's detail, go back
			if m.currentView == viewDetail && m.selectedSprite != nil && m.selectedSprite.Name == msg.name {
				m.currentView = viewDashboard
				m.selectedSprite = nil
			}
		}
		return m, m.fetchSprites

	case reconnectedMsg:
		// Daemon connection was broken and we reconnected. Swap the client
		// and immediately re-fetch sprites.
		if m.client != nil {
			m.client.Close()
		}
		m.client = msg.client
		m.err = nil
		m.message = "reconnected to daemon"
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

	// Update tag input if tagging
	if m.tagging {
		var cmd tea.Cmd
		m.tagInput, cmd = m.tagInput.Update(msg)
		return m, cmd
	}

	return m, nil
}

// handleKey processes keyboard input based on current view.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Delete confirmation intercepts all keys
	if m.confirmDelete {
		return m.handleDeleteConfirm(msg)
	}

	// Tag input intercepts all keys
	if m.tagging {
		return m.handleTagKey(msg)
	}

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
		// Add a tag to the selected sprite
		if s := m.selectedDashboardSprite(); s != nil {
			m.tagging = true
			m.tagTarget = s.Name
			m.tagRemoving = false
			m.tagInput.SetValue("")
			m.tagInput.Focus()
			return m, textinput.Blink
		}
	case "T":
		// Remove a tag from the selected sprite
		if s := m.selectedDashboardSprite(); s != nil {
			m.tagging = true
			m.tagTarget = s.Name
			m.tagRemoving = true
			m.tagInput.SetValue("")
			m.tagInput.Focus()
			return m, textinput.Blink
		}
	case "o":
		// Open sprite URL in browser
		if s := m.selectedDashboardSprite(); s != nil {
			return m, m.openInBrowser(s)
		}
	case "c":
		// Connect to sprite via interactive console
		if s := m.selectedDashboardSprite(); s != nil {
			return m, m.connectConsole(s)
		}
	case "s":
		// Toggle sync start/stop
		if s := m.selectedDashboardSprite(); s != nil {
			return m, m.toggleSync(s)
		}
	case "d":
		// Delete sprite (enter confirmation mode)
		if s := m.selectedDashboardSprite(); s != nil {
			m.confirmDelete = true
			m.deleteName = s.Name
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
	case "o":
		// Open sprite URL in browser
		if m.selectedSprite != nil {
			return m, m.openInBrowser(m.selectedSprite)
		}
	case "c":
		// Connect to sprite via interactive console
		if m.selectedSprite != nil {
			return m, m.connectConsole(m.selectedSprite)
		}
	case "s":
		// Toggle sync start/stop
		if m.selectedSprite != nil {
			return m, m.toggleSync(m.selectedSprite)
		}
	case "d":
		// Delete sprite (enter confirmation mode)
		if m.selectedSprite != nil {
			m.confirmDelete = true
			m.deleteName = m.selectedSprite.Name
		}
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

	// Tag input
	if m.tagging {
		action := "Add tag to"
		if m.tagRemoving {
			action = "Remove tag from"
		}
		b.WriteString(fmt.Sprintf("%s %s: ", action, m.tagTarget))
		b.WriteString(m.tagInput.View())
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

	// Delete confirmation banner
	if m.confirmDelete {
		b.WriteString("\n")
		b.WriteString(ErrorStyle.Render(fmt.Sprintf("Delete sprite %q? [y] confirm  [n/esc] cancel", m.deleteName)))
	}

	// Help bar
	b.WriteString("\n")
	help := "[↑↓/jk] navigate  [enter] details  [o] open  [c] console  [s] sync  [d] delete  [f] filter  [t/T] tag/untag  [r] refresh  [q] quit"
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

	// Delete confirmation banner
	if m.confirmDelete {
		b.WriteString("\n")
		b.WriteString(ErrorStyle.Render(fmt.Sprintf("Delete sprite %q? [y] confirm  [n/esc] cancel", m.deleteName)))
	}

	// Help
	b.WriteString("\n")
	help := "[esc] back  [o] open  [c] console  [s] sync  [d] delete  [r] refresh  [q] quit"
	b.WriteString(HelpStyle.Render(help))

	return b.String()
}

// fetchSprites is a tea.Cmd that queries the daemon for the current sprite list.
// reconnectedMsg carries a new daemon client after a successful reconnect.
type reconnectedMsg struct {
	client *daemon.Client
}

func (m Model) fetchSprites() tea.Msg {
	opts := store.ListOptions{
		Tags:       m.filterOpts.Tags,
		PathPrefix: m.filterOpts.PathPrefix,
		NameFilter: m.filterOpts.NameFilter,
	}

	sprites, err := m.client.ListSprites(opts)
	if err != nil {
		// Connection may be broken (daemon restarted). Try to reconnect.
		newClient, reconErr := daemon.Connect()
		if reconErr != nil {
			return errMsg{err: fmt.Errorf("daemon unavailable: %v (reconnect: %v)", err, reconErr)}
		}
		// Send reconnect message so Update can swap the client
		return reconnectedMsg{client: newClient}
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

// handleTagKey processes keys when the tag text input is active.
func (m Model) handleTagKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		tag := strings.TrimSpace(m.tagInput.Value())
		if tag == "" {
			m.tagging = false
			m.tagInput.Blur()
			return m, nil
		}
		m.tagging = false
		m.tagInput.Blur()
		name := m.tagTarget
		removing := m.tagRemoving
		return m, func() tea.Msg {
			if removing {
				if err := m.client.UntagSprite(name, tag); err != nil {
					return errMsg{err: err}
				}
				return msgMsg(fmt.Sprintf("Removed tag %q from %s", tag, name))
			}
			if err := m.client.TagSprite(name, tag); err != nil {
				return errMsg{err: err}
			}
			return msgMsg(fmt.Sprintf("Added tag %q to %s", tag, name))
		}
	case "esc":
		m.tagging = false
		m.tagInput.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.tagInput, cmd = m.tagInput.Update(msg)
	return m, cmd
}

// selectedDashboardSprite returns the sprite at the current cursor position, or nil.
func (m Model) selectedDashboardSprite() *store.Sprite {
	if len(m.sprites) > 0 && m.cursor < len(m.sprites) {
		return m.sprites[m.cursor]
	}
	return nil
}

// openInBrowser opens the sprite's public URL in the default browser.
func (m Model) openInBrowser(s *store.Sprite) tea.Cmd {
	return func() tea.Msg {
		url := s.URL
		if url == "" {
			return errMsg{err: fmt.Errorf("no URL for sprite %s", s.Name)}
		}
		if err := exec.Command("open", url).Start(); err != nil {
			return errMsg{err: fmt.Errorf("opening browser: %w", err)}
		}
		return msgMsg(fmt.Sprintf("Opened %s", url))
	}
}

// connectConsole suspends the TUI and opens an interactive console session to
// the sprite using `sprite exec`. When the session ends, the TUI resumes.
func (m Model) connectConsole(s *store.Sprite) tea.Cmd {
	name := s.Name
	org := s.Org
	remotePath := s.RemotePath
	if remotePath == "" {
		remotePath = "/home/sprite"
	}

	shellCmd := fmt.Sprintf(
		"mkdir -p '%s' && cd '%s' && exec tmux new-session -A -s console bash",
		remotePath, remotePath,
	)

	args := []string{"exec"}
	if org != "" {
		args = append(args, "-o", org)
	}
	args = append(args, "-s", name, "-tty", "sh", "-c", shellCmd)

	c := exec.Command("sprite", args...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		// Reset terminal mouse tracking modes that tmux may have left enabled
		fmt.Fprintf(os.Stderr, "\033[?1000l\033[?1003l\033[?1006l")
		return consoleFinishedMsg{err: err}
	})
}

// toggleSync starts or stops sync for a sprite depending on its current sync status.
func (m Model) toggleSync(s *store.Sprite) tea.Cmd {
	return func() tea.Msg {
		name := s.Name
		switch s.SyncStatus {
		case "watching", "syncing", "connecting", "recovering":
			// Sync is active — stop it
			if err := m.client.StopSync(name); err != nil {
				return syncToggledMsg{name: name, action: "stopped", err: err}
			}
			return syncToggledMsg{name: name, action: "stopped"}
		default:
			// Sync is not active — start it
			if s.LocalPath == "" {
				return syncToggledMsg{name: name, action: "started",
					err: fmt.Errorf("no local path configured for %s", name)}
			}
			req := daemon.StartSyncRequest{
				SpriteName: name,
				LocalPath:  s.LocalPath,
				RemotePath: s.RemotePath,
				Org:        s.Org,
			}
			if _, err := m.client.StartSync(req); err != nil {
				return syncToggledMsg{name: name, action: "started", err: err}
			}
			return syncToggledMsg{name: name, action: "started"}
		}
	}
}

// handleDeleteConfirm processes keys during delete confirmation mode.
func (m Model) handleDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		name := m.deleteName
		return m, m.deleteSprite(name)
	case "n", "N", "esc":
		m.confirmDelete = false
		m.deleteName = ""
	}
	return m, nil
}

// deleteSprite removes a sprite from both the daemon database and the remote
// Sprites API. Stops sync first if running.
func (m Model) deleteSprite(name string) tea.Cmd {
	return func() tea.Msg {
		// Stop sync first (ignore errors — may not be running)
		m.client.StopSync(name)

		// Delete from daemon database
		if err := m.client.DeleteSprite(name); err != nil {
			return deleteResultMsg{name: name, err: fmt.Errorf("deleting from database: %w", err)}
		}

		return deleteResultMsg{name: name}
	}
}
