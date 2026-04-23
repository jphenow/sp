package cmd

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/store"
)

// discoverCmd finds existing Mutagen sessions for sprites and offers to import
// any that aren't already tracked by the daemon.
var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Discover and import existing Mutagen sync sessions",
	Long: `Scans for active Mutagen sync sessions named 'sprite-*' that aren't
tracked by the daemon, and imports them into the dashboard.

This is useful when migrating from the Bash sp script or when sync sessions
were created outside of sp's daemon management.`,
	RunE: runDiscover,
}

func init() {
	rootCmd.AddCommand(discoverCmd)
}

// mutagenSessionInfo holds parsed info from mutagen sync list output.
type mutagenSessionInfo struct {
	Name       string
	Identifier string
	Alpha      string // local path
	Beta       string // remote path (user@host:path)
	Status     string
}

// runDiscover lists all sprite-* Mutagen sessions and imports untracked ones.
func runDiscover(cmd *cobra.Command, args []string) error {
	// List all Mutagen sessions
	sessions, err := listAllMutagenSessions()
	if err != nil {
		return fmt.Errorf("listing Mutagen sessions: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Println("No Mutagen sync sessions found.")
		return nil
	}

	// Connect to daemon
	dc, err := daemon.Connect()
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer dc.Close()

	// Get already-tracked sprites
	tracked, err := dc.ListSprites(store.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing tracked sprites: %w", err)
	}
	trackedNames := make(map[string]bool)
	for _, s := range tracked {
		trackedNames[s.Name] = true
	}

	// Find untracked sessions
	var untracked []mutagenSessionInfo
	for _, s := range sessions {
		// Session name format: sprite-<sprite-name>
		spriteName := strings.TrimPrefix(s.Name, "sprite-")
		if !trackedNames[spriteName] {
			untracked = append(untracked, s)
		}
	}

	if len(untracked) == 0 {
		fmt.Printf("Found %d Mutagen sessions — all already tracked.\n", len(sessions))
		return nil
	}

	fmt.Printf("Found %d Mutagen sessions, %d untracked:\n\n", len(sessions), len(untracked))
	for _, s := range untracked {
		spriteName := strings.TrimPrefix(s.Name, "sprite-")
		fmt.Printf("  %-35s %s -> %s [%s]\n", spriteName, s.Alpha, s.Beta, s.Status)
	}

	fmt.Printf("\nImporting %d sprite(s)...\n", len(untracked))

	imported := 0
	for _, s := range untracked {
		spriteName := strings.TrimPrefix(s.Name, "sprite-")

		// Try to import via daemon (fetches API info)
		result, err := dc.ImportSprite(spriteName, s.Alpha, nil)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  Warning: could not import %s: %v\n", spriteName, err)
			continue
		}

		fmt.Printf("  Imported: %s", result.Name)
		if result.URL != "" {
			fmt.Printf(" (%s)", result.URL)
		}
		fmt.Println()
		imported++
	}

	fmt.Printf("\nDone. Imported %d sprite(s).\n", imported)
	return nil
}

// listAllMutagenSessions runs `mutagen sync list` and parses all sprite-* sessions.
func listAllMutagenSessions() ([]mutagenSessionInfo, error) {
	out, err := exec.Command("mutagen", "sync", "list").Output()
	if err != nil {
		// No sessions or mutagen not installed
		return nil, nil
	}

	return parseMutagenList(string(out)), nil
}

// parseMutagenList parses the output of `mutagen sync list` (all sessions)
// and returns info for sessions whose name starts with "sprite-".
func parseMutagenList(output string) []mutagenSessionInfo {
	var sessions []mutagenSessionInfo
	var current *mutagenSessionInfo
	inAlpha := false
	inBeta := false

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)

		// New session block starts with a line like:
		// Name: sprite-gh-jphenow--gameservers
		if strings.HasPrefix(trimmed, "Name:") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "Name:"))
			if strings.HasPrefix(name, "sprite-") {
				if current != nil {
					sessions = append(sessions, *current)
				}
				current = &mutagenSessionInfo{Name: name}
			} else {
				// Not a sprite session — skip until next Name:
				if current != nil {
					sessions = append(sessions, *current)
				}
				current = nil
			}
			inAlpha = false
			inBeta = false
			continue
		}

		if current == nil {
			continue
		}

		if strings.HasPrefix(trimmed, "Identifier:") {
			current.Identifier = strings.TrimSpace(strings.TrimPrefix(trimmed, "Identifier:"))
		}

		if strings.HasPrefix(trimmed, "Status:") {
			current.Status = strings.TrimSpace(strings.TrimPrefix(trimmed, "Status:"))
		}

		// Track which endpoint section we're in
		if trimmed == "Alpha:" {
			inAlpha = true
			inBeta = false
			continue
		}
		if trimmed == "Beta:" {
			inAlpha = false
			inBeta = true
			continue
		}

		if strings.HasPrefix(trimmed, "URL:") {
			url := strings.TrimSpace(strings.TrimPrefix(trimmed, "URL:"))
			if inAlpha {
				current.Alpha = url
			} else if inBeta {
				current.Beta = url
			}
		}
	}

	// Don't forget the last session
	if current != nil {
		sessions = append(sessions, *current)
	}

	return sessions
}
