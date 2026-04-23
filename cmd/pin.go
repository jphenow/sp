package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
)

// pinCmd marks a variant sprite as graduated so `sp prune` skips it.
// The target can be a literal sprite name or an owner/repo:variant shorthand.
var pinCmd = &cobra.Command{
	Use:   "pin <sprite-or-variant>",
	Short: "Pin a variant sprite so sp prune won't sweep it",
	Long: `Pins a variant sprite. Pinned sprites are excluded from sp prune,
letting a short-run experiment graduate into long-lived work without
getting swept.

Only variant sprites can be pinned — non-variants are always preserved.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setPin(args[0], true)
	},
}

// unpinCmd reverses pinCmd. Unpinning makes the variant eligible for prune again.
var unpinCmd = &cobra.Command{
	Use:   "unpin <sprite-or-variant>",
	Short: "Unpin a variant sprite",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setPin(args[0], false)
	},
}

// setPin resolves the input to a sprite, validates it is a variant, and
// toggles the pinned flag via the daemon.
func setPin(input string, pinned bool) error {
	dc, err := daemon.Connect()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer dc.Close()

	name, err := resolveSpriteName(dc, input)
	if err != nil {
		return err
	}

	s, err := dc.GetSprite(name)
	if err != nil {
		return fmt.Errorf("looking up sprite: %w", err)
	}
	if s == nil {
		return fmt.Errorf("sprite %q not found", name)
	}
	if s.Variant == "" {
		return fmt.Errorf("sprite %q is not a variant; pinning is only meaningful for variants", name)
	}

	if err := dc.SetPinned(name, pinned); err != nil {
		return fmt.Errorf("setting pinned: %w", err)
	}

	if pinned {
		fmt.Printf("Pinned %s\n", name)
	} else {
		fmt.Printf("Unpinned %s\n", name)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(pinCmd)
	rootCmd.AddCommand(unpinCmd)
}
