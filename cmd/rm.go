package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/sprite"
)

var rmForce bool

// rmCmd destroys a sprite and removes it from the daemon's database.
// Refuses to destroy a non-variant sprite unless --force is passed — this
// is the primary safety rail against muscle-memory rm-ing a main sprite.
var rmCmd = &cobra.Command{
	Use:   "rm <sprite-or-variant>",
	Short: "Destroy a variant sprite and untrack it",
	Long: `Destroys a sprite via the sprite CLI and removes it from the sp
database.

Argument forms:
  sp rm gh-fly--flyctl--scratch-idea     full sprite name
  sp rm fly/flyctl:scratch-idea          owner/repo:variant shorthand

Safety:
  By default, sp rm refuses to destroy a non-variant sprite. Pass --force
  to override.`,
	Args: cobra.ExactArgs(1),
	RunE: runRm,
}

// runRm resolves the argument to a stored sprite, validates the safety
// rails, destroys the remote sprite, and deletes the local record.
func runRm(cmd *cobra.Command, args []string) error {
	dc, err := daemon.Connect()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer dc.Close()

	name, err := resolveSpriteName(dc, args[0])
	if err != nil {
		return err
	}

	s, err := dc.GetSprite(name)
	if err != nil {
		return fmt.Errorf("looking up sprite: %w", err)
	}
	if s == nil {
		return fmt.Errorf("sprite %q not found in sp database", name)
	}

	if s.Variant == "" && !rmForce {
		return fmt.Errorf("sprite %q is not a variant; pass --force to destroy it anyway", name)
	}

	if s.Pinned && !rmForce {
		return fmt.Errorf("sprite %q is pinned; unpin first or pass --force", name)
	}

	// Destroy the remote sprite. Done before the DB delete so a failure
	// leaves the record in place for a retry.
	client := sprite.NewClient(s.Org)
	if err := client.Destroy(name); err != nil {
		return fmt.Errorf("destroying sprite: %w", err)
	}

	if err := dc.DeleteSprite(name); err != nil {
		return fmt.Errorf("deleting from db: %w", err)
	}

	fmt.Printf("Removed %s\n", name)
	return nil
}

// resolveSpriteName accepts either a literal sprite name or the
// `owner/repo:variant` shorthand and returns the literal name.
// The shorthand maps to `gh-<owner>--<repo>--<sanitized-variant>`.
func resolveSpriteName(dc *daemon.Client, input string) (string, error) {
	// Check for owner/repo:variant shorthand first. We require both a "/"
	// (owner/repo) and a ":" (variant separator) to treat it as shorthand.
	if strings.Contains(input, "/") && strings.Contains(input, ":") {
		slash := strings.Index(input, "/")
		colon := strings.Index(input, ":")
		if colon > slash {
			owner := input[:slash]
			repo := input[slash+1 : colon]
			variant := input[colon+1:]
			if owner == "" || repo == "" || variant == "" {
				return "", fmt.Errorf("invalid shorthand %q: expected owner/repo:variant", input)
			}
			return fmt.Sprintf("gh-%s--%s--%s", owner, repo, sanitizeVariant(variant)), nil
		}
	}

	// Otherwise treat as literal name. Verify it exists.
	if _, err := dc.GetSprite(input); err != nil {
		return "", fmt.Errorf("looking up sprite %q: %w", input, err)
	}
	return input, nil
}

// sanitizeVariant mirrors the sanitization rules in setup.sanitizeName for
// variant labels. Kept in-sync manually; setup.sanitizeName is unexported.
func sanitizeVariant(s string) string {
	s = strings.ToLower(s)
	r := strings.NewReplacer(" ", "-", ".", "-", "_", "-", ":", "-")
	return r.Replace(s)
}

func init() {
	rmCmd.Flags().BoolVar(&rmForce, "force", false, "destroy even if the sprite is not a variant or is pinned")
	rootCmd.AddCommand(rmCmd)
}
