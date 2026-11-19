package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/progress"
	"github.com/jphenow/sp/internal/store"
)

// eventRetention is how long sp keeps diagnostics before pruning them.
//
// Long enough that "this has been happening for a couple of weeks" is still
// answerable, short enough that the database stays small and nobody has to
// think about it. Older rows are not worth keeping: the sprite, the session
// and usually the code have all moved on, so there is nothing left to
// investigate.
const eventRetention = 30 * 24 * time.Hour

// eventSprite is the sprite the current command is working on, used to tag
// warnings that do not know which sprite they are about. Set once the target
// is resolved; empty before that, which is honest — a warning from argument
// parsing belongs to no sprite.
var eventSprite string

func init() {
	// Every warning sp prints is kept, whoever printed it. This is the whole
	// point: the warnings worth investigating are the ones that scroll past
	// while a session is starting.
	progress.WarnSink = func(source, msg string) {
		if source == "" {
			source = "setup"
		}
		recordEvent(store.Event{Level: "warn", Source: source, Message: msg})
	}
}

// warnf prints a warning and keeps it, for the warnings that go straight to
// stderr rather than through the progress package.
func warnf(source, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "Warning: %s\n", msg)
	recordEvent(store.Event{Level: "warn", Source: source, Message: msg})
}

// recordEvent keeps one event, filling in the sprite the command is working
// on. Best-effort and quiet: this is a diagnostic aid, and a failure to write
// one is not worth a second warning on top of the first.
func recordEvent(e store.Event) {
	if e.Sprite == "" {
		e.Sprite = eventSprite
	}
	db, err := store.Open()
	if err != nil {
		slog.Debug("recording event", "err", err)
		return
	}
	defer db.Close()
	if err := db.RecordEvent(e); err != nil {
		slog.Debug("recording event", "err", err)
	}
}

// Flags for `sp events`.
var (
	eventDays   int
	eventLevel  string
	eventSource string
	eventLimit  int
	eventPrune  bool
)

// eventsCmd shows the warnings sp has kept.
var eventsCmd = &cobra.Command{
	Use:   "events [sprite]",
	Short: "Show warnings and errors sp has recorded",
	Long: `Lists the warnings sp printed, newest first.

sp's warnings arrive while a session is starting and are then scrolled past,
or erased with the alternate screen — so a problem that only happens
sometimes is hard to catch in the act. They are kept here instead, for ` + fmt.Sprintf("%d", int(eventRetention.Hours()/24)) + ` days.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var sprite string
		if len(args) == 1 {
			sprite = args[0]
		}
		return showEvents(sprite, eventDays)
	},
}

func init() {
	eventsCmd.Flags().IntVar(&eventDays, "days", 7, "how many days back to show")
	eventsCmd.Flags().StringVar(&eventLevel, "level", "", "only this level (warn, error)")
	eventsCmd.Flags().StringVar(&eventSource, "source", "", "only this source (e.g. claude-config, sync, hold)")
	eventsCmd.Flags().IntVar(&eventLimit, "limit", 50, "most rows to show (0 for all)")
	eventsCmd.Flags().BoolVar(&eventPrune, "prune", false, "delete everything past the retention window and exit")
	rootCmd.AddCommand(eventsCmd)
}

// showEvents prints the kept events, or prunes them.
func showEvents(sprite string, days int) error {
	db, err := store.Open()
	if err != nil {
		return fmt.Errorf("opening the sp database: %w", err)
	}
	defer db.Close()

	if eventPrune {
		n, err := db.Prune(time.Now().Add(-eventRetention))
		if err != nil {
			return err
		}
		fmt.Printf("Pruned %d rows older than %s.\n", n, eventRetention)
		return nil
	}

	events, err := db.Events(time.Now().AddDate(0, 0, -days), store.EventFilter{
		Sprite: sprite,
		Level:  eventLevel,
		Source: eventSource,
		Limit:  eventLimit,
	})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Printf("Nothing recorded in the last %d days.\n", days)
		return nil
	}

	for _, e := range events {
		line := fmt.Sprintf("%s  %-5s %-14s", e.At.Local().Format("Jan 02 15:04"), e.Level, e.Source)
		if e.Sprite != "" {
			line += " " + e.Sprite
		}
		fmt.Printf("%s\n    %s\n", line, strings.TrimSpace(e.Message))
		if e.Detail != "" {
			fmt.Printf("    %s\n", strings.TrimSpace(e.Detail))
		}
	}
	fmt.Printf("\n%d events. Counts by source:\n", len(events))
	for _, c := range countEventSources(events) {
		fmt.Printf("  %-16s %d\n", c.source, c.count)
	}
	return nil
}

// sourceCount is one row of the per-source tally.
type sourceCount struct {
	source string
	count  int
}

// countEventSources tallies events by source, busiest first. A recurring
// problem is easier to see as a count than as a list of near-identical lines.
func countEventSources(events []store.Event) []sourceCount {
	counts := map[string]int{}
	var order []string
	for _, e := range events {
		if _, seen := counts[e.Source]; !seen {
			order = append(order, e.Source)
		}
		counts[e.Source]++
	}
	out := make([]sourceCount, 0, len(order))
	for _, s := range order {
		out = append(out, sourceCount{source: s, count: counts[s]})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].count > out[j-1].count; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
