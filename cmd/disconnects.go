package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/store"
)

// disconnectDays is the window `sp disconnects` reports on.
var disconnectDays int

// disconnectsCmd reports how interactive sessions have been ending, per sprite.
//
// It exists because "this sprite feels flakier" is not something sp could
// answer: every attempt was forgotten the moment it ended, so the only record
// was an impression. Comparing two sprites needs counts.
var disconnectsCmd = &cobra.Command{
	Use:   "disconnects [sprite]",
	Short: "Show how sessions have been ending, per sprite",
	Long: `Summarizes every interactive session attempt sp has made, grouped by sprite.

WAKE is the time an attach took to print anything. Attaching to a sprite that
is awake paints its tmux session at once; one that has to be woken first takes
seconds, so a high median wake means that sprite is idle-pausing underneath
you — which is the usual reason a session seems to drop more often.

Outcomes:
  clean    the session ended on its own
  dropped  the connection failed and the client said so
  stall    the connection stopped answering input and sp killed it
  ended    the client exited non-zero for some other reason`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var sprite string
		if len(args) == 1 {
			sprite = args[0]
		}
		return showDisconnects(sprite, disconnectDays)
	},
}

func init() {
	disconnectsCmd.Flags().IntVar(&disconnectDays, "days", 30, "how many days back to report on")
	rootCmd.AddCommand(disconnectsCmd)
}

// showDisconnects prints the per-sprite summary and the most recent attempts.
func showDisconnects(sprite string, days int) error {
	db, err := store.Open()
	if err != nil {
		return fmt.Errorf("opening the sp database: %w", err)
	}
	defer db.Close()

	since := time.Now().AddDate(0, 0, -days)
	attempts, err := db.Attempts(since, sprite)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		fmt.Printf("No session attempts recorded in the last %d days.\n", days)
		return nil
	}

	fmt.Printf("Session attempts, last %d days\n\n", days)
	fmt.Printf("%-32s %8s %6s %8s %6s %6s %10s %10s %8s\n",
		"SPRITE", "ATTEMPTS", "CLEAN", "DROPPED", "STALL", "ENDED", "MEDIAN UP", "LONGEST UP", "WAKE")
	fmt.Println(strings.Repeat("-", 112))
	for _, s := range summarizeAttempts(attempts) {
		fmt.Printf("%-32s %8d %6d %8d %6d %6d %10s %10s %8s\n",
			s.sprite, s.total, s.outcomes["clean"], s.outcomes["dropped"],
			s.outcomes["stall"], s.outcomes["ended"],
			shortDuration(s.medianUptime), shortDuration(s.longestUptime),
			shortDuration(s.medianWake))
	}

	const recent = 15
	fmt.Printf("\nMost recent\n")
	for i, a := range attempts {
		if i == recent {
			break
		}
		line := fmt.Sprintf("%s  %-24s %-7s %-8s up %-8s wake %-7s attempt %d",
			a.At.Local().Format("Jan 02 15:04"), a.Sprite, a.Transport, a.Outcome,
			shortDuration(a.Uptime), shortDuration(a.FirstOutput), a.Attempt)
		if a.Detail != "" {
			line += "  " + a.Detail
		}
		fmt.Println(line)
	}
	return nil
}

// attemptSummary is one sprite's row in the summary table.
type attemptSummary struct {
	sprite        string
	total         int
	outcomes      map[string]int
	medianUptime  time.Duration
	longestUptime time.Duration
	// medianWake only counts attempts that printed something. An attempt that
	// never printed never connected, and folding those in as zero would hide
	// exactly the slow wakes this column exists to show.
	medianWake time.Duration
}

// summarizeAttempts groups attempts by sprite, busiest first.
func summarizeAttempts(attempts []store.Attempt) []attemptSummary {
	type acc struct {
		outcomes map[string]int
		uptimes  []time.Duration
		wakes    []time.Duration
	}
	byName := map[string]*acc{}
	var order []string
	for _, a := range attempts {
		s, ok := byName[a.Sprite]
		if !ok {
			s = &acc{outcomes: map[string]int{}}
			byName[a.Sprite] = s
			order = append(order, a.Sprite)
		}
		s.outcomes[a.Outcome]++
		s.uptimes = append(s.uptimes, a.Uptime)
		if a.FirstOutput > 0 {
			s.wakes = append(s.wakes, a.FirstOutput)
		}
	}

	out := make([]attemptSummary, 0, len(order))
	for _, name := range order {
		s := byName[name]
		out = append(out, attemptSummary{
			sprite:        name,
			total:         len(s.uptimes),
			outcomes:      s.outcomes,
			medianUptime:  median(s.uptimes),
			longestUptime: longest(s.uptimes),
			medianWake:    median(s.wakes),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].total != out[j].total {
			return out[i].total > out[j].total
		}
		return out[i].sprite < out[j].sprite
	})
	return out
}

// median returns the middle duration, or 0 for an empty set.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// longest returns the largest duration, or 0 for an empty set.
func longest(ds []time.Duration) time.Duration {
	var biggest time.Duration
	for _, d := range ds {
		if d > biggest {
			biggest = d
		}
	}
	return biggest
}

// shortDuration formats a duration at the one or two units that matter, so a
// column of them can be compared at a glance. "-" for zero, which means the
// measurement is missing rather than instant.
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
