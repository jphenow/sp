package tui

import "github.com/charmbracelet/lipgloss"

// Color palette for the TUI.
var (
	colorPrimary   = lipgloss.Color("#7C3AED") // purple
	colorSecondary = lipgloss.Color("#6366F1") // indigo
	colorSuccess   = lipgloss.Color("#10B981") // green
	colorWarning   = lipgloss.Color("#F59E0B") // amber
	colorDanger    = lipgloss.Color("#EF4444") // red
	colorMuted     = lipgloss.Color("#6B7280") // gray
	colorCold      = lipgloss.Color("#3B82F6") // blue
	colorText      = lipgloss.Color("#E5E7EB") // light gray
)

// StatusStyle returns the styled string for a sprite status.
func StatusStyle(status string) string {
	switch status {
	case "running":
		return lipgloss.NewStyle().Foreground(colorSuccess).Render("running")
	case "warm":
		return lipgloss.NewStyle().Foreground(colorWarning).Render("warm")
	case "cold":
		return lipgloss.NewStyle().Foreground(colorCold).Render("cold")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render(status)
	}
}

// SyncStatusStyle returns the styled string for a sync status.
func SyncStatusStyle(status string) string {
	switch status {
	case "watching":
		return lipgloss.NewStyle().Foreground(colorSuccess).Render("watching")
	case "syncing":
		return lipgloss.NewStyle().Foreground(colorSecondary).Render("syncing")
	case "connecting":
		return lipgloss.NewStyle().Foreground(colorWarning).Render("connecting")
	case "error":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("error")
	case "idle":
		return lipgloss.NewStyle().Foreground(colorCold).Render("idle")
	case "recovering":
		return lipgloss.NewStyle().Foreground(colorWarning).Render("recover")
	case "disconnected":
		return lipgloss.NewStyle().Foreground(colorMuted).Render("disconn")
	case "none", "":
		return lipgloss.NewStyle().Foreground(colorMuted).Render("-")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render(status)
	}
}

// Shared styles for consistent UI elements.
var (
	// Header is the style for the top title bar.
	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPrimary).
			MarginBottom(1)

	// HelpStyle is the style for the bottom help bar.
	HelpStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			MarginTop(1)

	// SelectedRowStyle highlights the currently selected row.
	SelectedRowStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorText)

	// NormalRowStyle is the default row style.
	NormalRowStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	// TagStyle is the style for tag labels.
	TagStyle = lipgloss.NewStyle().
			Foreground(colorSecondary)

	// ErrorStyle is the style for error messages.
	ErrorStyle = lipgloss.NewStyle().
			Foreground(colorDanger)

	// DetailLabelStyle is for labels in the detail view.
	DetailLabelStyle = lipgloss.NewStyle().
				Foreground(colorMuted).
				Width(16)

	// DetailValueStyle is for values in the detail view.
	DetailValueStyle = lipgloss.NewStyle().
				Foreground(colorText)

	// BorderStyle wraps content in a rounded border.
	BorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)
)
