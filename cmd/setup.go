package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/store"
)

var setupAll bool

// setupCmd re-runs setup.conf against one or all sprites.
var setupCmd = &cobra.Command{
	Use:   "setup [target]",
	Short: "Re-run setup.conf (files and commands) on a sprite",
	Long: `Re-pushes files and re-runs commands from ~/.config/sprite/setup.conf
against a sprite. This is useful when you've updated local auth tokens,
dotfiles, or tool configurations and want to push them to your sprites.

Use --all to push to every tracked running sprite.
Target defaults to "." (current directory).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSetup,
}

func init() {
	setupCmd.Flags().BoolVar(&setupAll, "all", false, "run setup on all tracked running sprites")
	rootCmd.AddCommand(setupCmd)
}

// runSetup re-runs setup.conf against one sprite or all tracked running sprites.
func runSetup(cmd *cobra.Command, args []string) error {
	dc, err := daemon.Connect()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer dc.Close()

	if setupAll {
		return runSetupAll(dc)
	}

	// Single sprite mode
	resolved, err := resolveTarget(args)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}

	// Verify sprite exists in daemon
	if _, err := dc.GetSprite(resolved.SpriteName); err != nil {
		return fmt.Errorf("sprite %q not found: %w", resolved.SpriteName, err)
	}

	fmt.Printf("Running setup.conf on %s...\n", resolved.SpriteName)

	msg, err := dc.RunSetup(resolved.SpriteName)
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}

	fmt.Println(msg)
	return nil
}

// runSetupAll iterates through all tracked running sprites and runs setup.conf on each.
func runSetupAll(dc *daemon.Client) error {
	sprites, err := dc.ListSprites(store.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing sprites: %w", err)
	}

	var running []*store.Sprite
	for _, s := range sprites {
		if s.Status == "running" {
			running = append(running, s)
		}
	}

	if len(running) == 0 {
		fmt.Println("No tracked running sprites found.")
		return nil
	}

	fmt.Printf("Running setup.conf on %d tracked sprite(s)...\n", len(running))

	for _, s := range running {
		fmt.Printf("  %s... ", s.Name)
		msg, err := dc.RunSetup(s.Name)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			continue
		}
		fmt.Println(msg)
	}

	return nil
}
