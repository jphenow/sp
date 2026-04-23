package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/sprite"
	"github.com/jphenow/sp/internal/store"
)

var (
	pruneOlderThan time.Duration
	pruneYes       bool
	pruneAll       bool
)

// pruneCmd lists stale unpinned variant sprites and optionally destroys them.
// Dry-run by default: prints the candidate set and exits. Pass --yes to actually
// destroy them. Pinned variants are always excluded.
var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Show (or destroy) stale unpinned variant sprites",
	Long: `Lists variant sprites that are candidates for cleanup: unpinned
variants whose last update was more than --older-than ago.

Dry-run by default — no sprites are touched. Pass --yes to actually
destroy them.

Flags:
  --older-than  age threshold (default 14d, accepts any Go duration like 72h)
  --yes         actually destroy the listed sprites (default: dry-run)
  --all         ignore the age threshold; consider every unpinned variant

Pinned variants are always excluded regardless of age. Use "sp pin" to
protect an experiment you want to keep.`,
	Args: cobra.NoArgs,
	RunE: runPrune,
}

// runPrune queries the daemon for stale variants and either prints them
// (dry-run) or walks through destroying each one.
func runPrune(cmd *cobra.Command, args []string) error {
	dc, err := daemon.Connect()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer dc.Close()

	opts := store.ListOptions{
		OnlyVariants: true,
		OnlyUnpinned: true,
	}
	if !pruneAll {
		opts.OlderThan = time.Now().Add(-pruneOlderThan)
	}

	sprites, err := dc.ListSprites(opts)
	if err != nil {
		return fmt.Errorf("listing sprites: %w", err)
	}

	if len(sprites) == 0 {
		if pruneAll {
			fmt.Println("No unpinned variant sprites to prune.")
		} else {
			fmt.Printf("No unpinned variant sprites older than %s.\n", pruneOlderThan)
		}
		return nil
	}

	// Print the candidate table regardless of dry-run vs execute.
	fmt.Printf("Candidates (%d):\n", len(sprites))
	fmt.Printf("  %-40s %-16s %s\n", "NAME", "VARIANT", "AGE")
	fmt.Println("  " + strings.Repeat("-", 80))
	now := time.Now()
	for _, s := range sprites {
		age := now.Sub(s.UpdatedAt).Round(time.Minute)
		fmt.Printf("  %-40s %-16s %s\n", s.Name, s.Variant, age)
	}

	if !pruneYes {
		fmt.Println()
		fmt.Println("Dry run. Pass --yes to actually destroy these sprites.")
		return nil
	}

	// Interactive final confirmation even with --yes, unless stdin is not a TTY.
	// This is belt-and-suspenders because destruction is irreversible.
	if isTerminal() {
		fmt.Printf("\nDestroy %d sprite(s)? [y/N] ", len(sprites))
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), "y") {
			fmt.Println("Aborted.")
			return nil
		}
	}

	var failed []string
	for _, s := range sprites {
		client := sprite.NewClient(s.Org)
		if err := client.Destroy(s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: destroy failed: %v\n", s.Name, err)
			failed = append(failed, s.Name)
			continue
		}
		if err := dc.DeleteSprite(s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: db delete failed: %v\n", s.Name, err)
			failed = append(failed, s.Name)
			continue
		}
		fmt.Printf("  removed %s\n", s.Name)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d sprite(s) failed to prune: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// isTerminal reports whether stdin is connected to a terminal. A minimal
// stand-in so prune can skip interactive confirmation in scripts.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func init() {
	pruneCmd.Flags().DurationVar(&pruneOlderThan, "older-than", 14*24*time.Hour, "age threshold for pruning")
	pruneCmd.Flags().BoolVar(&pruneYes, "yes", false, "actually destroy the listed sprites (default: dry-run)")
	pruneCmd.Flags().BoolVar(&pruneAll, "all", false, "ignore the age threshold; consider every unpinned variant")
	rootCmd.AddCommand(pruneCmd)
}
