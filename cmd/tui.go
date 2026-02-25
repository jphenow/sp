package cmd

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/tui"
)

var (
	filterTags   string
	filterPrefix string
)

// tuiCmd opens the TUI dashboard for monitoring all sprites.
var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the TUI dashboard for monitoring all sprites",
	Long: `Opens an interactive terminal UI showing all your sprite environments,
their status (running/warm/cold), and sync state.

Use --filter to show only sprites with specific tags, or --prefix to filter
by local directory path.

Multiple TUI instances can run simultaneously with different filters.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Connect to daemon
		dc, err := daemon.Connect()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer dc.Close()

		// Build filter options
		opts := tui.FilterOptions{}
		if filterTags != "" {
			opts.Tags = strings.Split(filterTags, ",")
		}
		if filterPrefix != "" {
			opts.PathPrefix = filterPrefix
		}

		// Create and run TUI
		model := tui.NewModel(dc, opts)
		p := tea.NewProgram(model, tea.WithAltScreen())

		if _, err := p.Run(); err != nil {
			return fmt.Errorf("running TUI: %w", err)
		}

		return nil
	},
}

func init() {
	tuiCmd.Flags().StringVar(&filterTags, "filter", "", "filter by tags (comma-separated)")
	tuiCmd.Flags().StringVar(&filterPrefix, "prefix", "", "filter by local path prefix")
	rootCmd.AddCommand(tuiCmd)
}
