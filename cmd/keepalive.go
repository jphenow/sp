package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/keepalive"
	"github.com/jphenow/sp/internal/sprite"
)

var (
	keepAliveFor  time.Duration
	keepAliveStop bool
)

// keepaliveCmd holds a sprite in the Active state using the sprite Tasks API, so
// it doesn't idle-pause while you're working over the terminal or Remote
// Control. A held sprite never drops its connection or cold-restarts, which is
// what makes a session reliably reachable (including from a phone) without a
// manual wake.
var keepaliveCmd = &cobra.Command{
	Use:   "keepalive [target] [variant]",
	Short: "Hold a sprite Active (Tasks API) so it doesn't idle-pause while you work",
	Long: `keepalive registers a heartbeat against the sprite Tasks API so the sprite
stays in the Active state instead of pausing after ~30s idle. Use it when you
want a session to stay reachable without a manual wake — e.g. while driving it
from your phone over Remote Control.

  sp keepalive .                 # hold current dir's sprite (default 1h)
  sp keepalive owner/repo --for 2h
  sp keepalive owner/repo blue   # hold the 'blue' variant
  sp keepalive . --stop          # release the hold; sprite may pause again

The hold runs on the sprite and self-releases at the deadline, so a forgotten
keepalive stops billing on its own.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runKeepalive,
}

func init() {
	keepaliveCmd.Flags().DurationVar(&keepAliveFor, "for", time.Hour, "how long to hold the sprite Active (e.g. 30m, 2h)")
	keepaliveCmd.Flags().BoolVar(&keepAliveStop, "stop", false, "release the hold instead of starting it")
	rootCmd.AddCommand(keepaliveCmd)
}

// runKeepalive resolves the target sprite and either releases the hold (--stop)
// or launches a bounded heartbeat that keeps it Active for --for.
func runKeepalive(cmd *cobra.Command, args []string) error {
	resolved, err := resolveTarget(args)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}
	client := sprite.NewClient(resolved.Org)
	_, exists, err := adoptExistingSprite(client, resolved)
	if err != nil {
		return fmt.Errorf("checking sprite: %w", err)
	}
	if !exists {
		return fmt.Errorf("sprite %q does not exist — run 'sp %s' first", resolved.SpriteName, strings.Join(args, " "))
	}

	if keepAliveStop {
		keepalive.Stop(client, resolved.SpriteName, resolved.Org)
		fmt.Printf("Released keepalive hold on sprite %q.\n", resolved.SpriteName)
		return nil
	}

	// No idle or watch session: an explicit keepalive holds for the full window
	// regardless of terminal activity (that's the point of the command).
	if err := keepalive.Start(keepalive.Options{
		Sprite: resolved.SpriteName,
		Org:    resolved.Org,
		For:    keepAliveFor,
	}); err != nil {
		return fmt.Errorf("starting keepalive: %w", err)
	}
	fmt.Printf("Holding sprite %q Active for %s (self-releases after).\n", resolved.SpriteName, keepAliveFor)
	fmt.Printf("Release early: sp keepalive %s --stop\n", strings.Join(args, " "))
	return nil
}
