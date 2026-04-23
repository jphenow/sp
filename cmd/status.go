package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

var (
	statusOnlyVariants bool
	statusVariantsOf   string
)

// statusCmd shows the status of sprites.
var statusCmd = &cobra.Command{
	Use:   "status [sprite-name]",
	Short: "Show sprite status and sync health",
	Long: `Shows the status of a specific sprite or all tracked sprites.

Without arguments, shows a summary of all tracked sprites.
With a sprite name, shows detailed status including sync information.

Filtering flags:
  --variants               show only variant sprites (ones spawned via "sp . foo")
  --variants-of <base>     show only variants whose base matches <base>, e.g.
                           "sp status --variants-of gh-fly--flyctl"`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dc, err := daemon.Connect()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer dc.Close()

		if len(args) == 1 {
			return showSpriteStatus(dc, args[0])
		}
		return showAllStatus(dc, store.ListOptions{
			OnlyVariants: statusOnlyVariants || statusVariantsOf != "",
			VariantsOf:   statusVariantsOf,
		})
	},
}

// showSpriteStatus displays detailed status for a single sprite.
func showSpriteStatus(dc *daemon.Client, name string) error {
	s, err := dc.GetSprite(name)
	if err != nil {
		return fmt.Errorf("getting sprite: %w", err)
	}

	fmt.Printf("Sprite: %s\n", s.Name)
	fmt.Printf("  Status:      %s\n", s.Status)
	fmt.Printf("  Sync:        %s\n", s.SyncStatus)
	if s.SyncError != "" {
		fmt.Printf("  Sync Error:  %s\n", s.SyncError)
	}
	fmt.Printf("  URL:         %s\n", s.URL)
	fmt.Printf("  Local:       %s\n", s.LocalPath)
	fmt.Printf("  Remote:      %s\n", s.RemotePath)
	fmt.Printf("  Repo:        %s\n", s.Repo)

	// Check live Mutagen status
	mutagenState, err := spSync.GetMutagenStatus(name)
	if err == nil {
		fmt.Printf("\nMutagen Session:\n")
		fmt.Printf("  ID:             %s\n", mutagenState.MutagenID)
		fmt.Printf("  Status:         %s\n", mutagenState.Status)
		fmt.Printf("  Alpha:          connected=%v\n", mutagenState.AlphaConnected)
		fmt.Printf("  Beta:           connected=%v\n", mutagenState.BetaConnected)
		fmt.Printf("  Conflicts:      %d\n", mutagenState.Conflicts)
		if mutagenState.LastError != "" {
			fmt.Printf("  Last Error:     %s\n", mutagenState.LastError)
		}
	}

	tags, err := dc.GetTags(name)
	if err == nil && len(tags) > 0 {
		fmt.Printf("\nTags: %s\n", strings.Join(tags, ", "))
	}

	return nil
}

// showAllStatus displays a summary table of all tracked sprites.
func showAllStatus(dc *daemon.Client, opts store.ListOptions) error {
	sprites, err := dc.ListSprites(opts)
	if err != nil {
		return fmt.Errorf("listing sprites: %w", err)
	}

	if len(sprites) == 0 {
		if opts.OnlyVariants || opts.VariantsOf != "" {
			fmt.Println("No variant sprites match.")
			return nil
		}
		fmt.Println("No tracked sprites. Use 'sp import' to add existing sprites.")
		return nil
	}

	// Decide whether to show the VARIANT column. If any matching sprite has a
	// variant, it's worth the column. For explicit variant filtering, always show.
	showVariant := opts.OnlyVariants || opts.VariantsOf != ""
	if !showVariant {
		for _, s := range sprites {
			if s.Variant != "" {
				showVariant = true
				break
			}
		}
	}

	if showVariant {
		fmt.Printf("%-35s %-10s %-12s %-16s %-6s %s\n",
			"NAME", "STATUS", "SYNC", "VARIANT", "PINNED", "LOCAL PATH")
		fmt.Println(strings.Repeat("-", 110))
		for _, s := range sprites {
			localPath := trimPath(s.LocalPath, 30)
			variant := s.Variant
			if variant == "" {
				variant = "-"
			}
			pinned := ""
			if s.Pinned {
				pinned = "yes"
			}
			fmt.Printf("%-35s %-10s %-12s %-16s %-6s %s\n",
				s.Name, s.Status, s.SyncStatus, variant, pinned, localPath)
		}
		return nil
	}

	fmt.Printf("%-35s %-10s %-12s %s\n", "NAME", "STATUS", "SYNC", "LOCAL PATH")
	fmt.Println(strings.Repeat("-", 90))
	for _, s := range sprites {
		fmt.Printf("%-35s %-10s %-12s %s\n",
			s.Name, s.Status, s.SyncStatus, trimPath(s.LocalPath, 35))
	}
	return nil
}

// trimPath shortens a path for column display, ellipsizing the prefix when
// the path exceeds max. Returns the path unchanged when it fits.
func trimPath(p string, max int) string {
	if len(p) <= max {
		return p
	}
	return "..." + p[len(p)-(max-3):]
}

func init() {
	statusCmd.Flags().BoolVar(&statusOnlyVariants, "variants", false, "only show variant sprites")
	statusCmd.Flags().StringVar(&statusVariantsOf, "variants-of", "", "only show variants of the given base sprite name")
	rootCmd.AddCommand(statusCmd)
}
