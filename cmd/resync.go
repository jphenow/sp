package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/sprite"
)

// resyncCmd tears down and restarts sync for a sprite.
var resyncCmd = &cobra.Command{
	Use:   "resync [target]",
	Short: "Reset file sync for a sprite (flush, tear down, restart)",
	Long: `Tears down the current Mutagen sync session and restarts it fresh.
This flushes pending changes, kills the proxy, removes SSH config,
and re-establishes the full sync pipeline.

Target defaults to "." (current directory).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Resolve target to a sprite name
		resolved, err := resolveTarget(args)
		if err != nil {
			return fmt.Errorf("resolving target: %w", err)
		}

		dc, err := daemon.Connect()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer dc.Close()

		// Verify sprite exists in daemon
		s, err := dc.GetSprite(resolved.SpriteName)
		if err != nil {
			return fmt.Errorf("sprite %q not found: %w", resolved.SpriteName, err)
		}

		// Use stored paths, but allow override from resolved target
		localPath := s.LocalPath
		if resolved.LocalPath != "" {
			localPath = resolved.LocalPath
		}
		remotePath := s.RemotePath
		if resolved.RemotePath != "" {
			remotePath = resolved.RemotePath
		}

		if localPath == "" {
			return fmt.Errorf("no local path configured for %s — use 'sp import --path' first", resolved.SpriteName)
		}

		fmt.Printf("Resetting sync for %s...\n", resolved.SpriteName)

		// Resync via daemon: flush + stop + start
		if err := dc.Resync(resolved.SpriteName, localPath, remotePath, s.Org); err != nil {
			return fmt.Errorf("resync failed: %w", err)
		}

		fmt.Println("Sync restarting in background.")
		return nil
	},
}

// sessionsCmd lists active tmux sessions on a sprite.
var sessionsCmd = &cobra.Command{
	Use:   "sessions [target]",
	Short: "List active tmux sessions on a sprite",
	Long: `Lists all active tmux sessions inside a sprite environment.

Target defaults to "." (current directory).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resolved, err := resolveTarget(args)
		if err != nil {
			return fmt.Errorf("resolving target: %w", err)
		}

		client := sprite.NewClient(resolved.Org)
		out, err := client.Sessions(resolved.SpriteName)
		if err != nil {
			fmt.Println("No active tmux sessions")
			return nil
		}

		fmt.Print(out)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(resyncCmd)
	rootCmd.AddCommand(sessionsCmd)
}
