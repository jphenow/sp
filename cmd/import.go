package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
)

var (
	importPath string
	importTags string
)

// importCmd imports an existing sprite into the local database for monitoring.
var importCmd = &cobra.Command{
	Use:   "import <sprite-name>",
	Short: "Import an existing sprite into the dashboard",
	Long: `Import an existing sprite (from 'sprite list') into the sp dashboard
so it appears in the TUI and is monitored for health.

Optionally specify the local directory and tags for filtering.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		dc, err := daemon.Connect()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer dc.Close()

		var tags []string
		if importTags != "" {
			tags = strings.Split(importTags, ",")
		}

		s, err := dc.ImportSprite(name, importPath, tags)
		if err != nil {
			return fmt.Errorf("importing sprite: %w", err)
		}

		fmt.Printf("Imported sprite: %s\n", s.Name)
		if s.URL != "" {
			fmt.Printf("  URL:    %s\n", s.URL)
		}
		if s.Status != "" {
			fmt.Printf("  Status: %s\n", s.Status)
		}
		if s.LocalPath != "" {
			fmt.Printf("  Local:  %s\n", s.LocalPath)
		}
		if len(tags) > 0 {
			fmt.Printf("  Tags:   %s\n", strings.Join(tags, ", "))
		}

		return nil
	},
}

func init() {
	importCmd.Flags().StringVar(&importPath, "path", "", "local directory path for this sprite")
	importCmd.Flags().StringVar(&importTags, "tag", "", "tags to apply (comma-separated)")
	rootCmd.AddCommand(importCmd)
}
