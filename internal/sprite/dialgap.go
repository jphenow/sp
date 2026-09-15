package sprite

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// slowCallThreshold is how long a sprite CLI call must take before its debug
// log is worth analyzing. Healthy calls return in ~0.2s.
const slowCallThreshold = 5 * time.Second

// debugTimestamp matches the leading `time=<RFC3339>` on a sprite CLI debug
// line, and captures the message that follows.
var debugTimestamp = regexp.MustCompile(`^time=(\S+) level=\w+ msg=("[^"]*"|\S+)`)

// analyzeDialGap finds the largest pause between consecutive lines of a sprite
// CLI debug log and describes it.
//
// This automates the analysis that identified the problem in the first place:
// on a 135s call, ALL of it sat in a single gap between "control disabled by
// client option" and "Command started connectionMode=direct" — i.e. entirely in
// connection establishment, with the command itself finishing in 0.02s. Knowing
// which two log lines bracket the pause is what separates "the sprite is slow"
// from "the dial is slow", and they need completely different responses.
//
// Returns "" if the log is unreadable or has no gap worth reporting.
func analyzeDialGap(path string, minGap time.Duration) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	type entry struct {
		at  time.Time
		msg string
	}
	var entries []entry
	for _, line := range strings.Split(string(data), "\n") {
		m := debugTimestamp.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			continue
		}
		entries = append(entries, entry{at: at, msg: strings.Trim(m[2], `"`)})
	}
	if len(entries) < 2 {
		return ""
	}

	best := time.Duration(0)
	bestAt := 0
	for i := 1; i < len(entries); i++ {
		if d := entries[i].at.Sub(entries[i-1].at); d > best {
			best, bestAt = d, i
		}
	}
	if best < minGap {
		return ""
	}
	return fmt.Sprintf("%s stalled between %q and %q",
		best.Round(time.Millisecond), entries[bestAt-1].msg, entries[bestAt].msg)
}

// withDebugLog allocates a temp file for the sprite CLI's --debug output and
// returns the flag to pass plus a cleanup/analysis function. The analysis runs
// only when the call was slow — otherwise the log is just deleted, so the
// healthy path costs one temp file and nothing else.
func withDebugLog() (flag string, finish func(dur time.Duration) string) {
	f, err := os.CreateTemp("", "sp-sprite-debug-*.log")
	if err != nil {
		return "", func(time.Duration) string { return "" }
	}
	path := f.Name()
	f.Close()
	return "--debug=" + path, func(dur time.Duration) string {
		defer os.Remove(path)
		if dur < slowCallThreshold {
			return ""
		}
		// Only report a gap that accounts for most of the call; a small one
		// is noise, and the case worth surfacing is "one pause ate everything".
		return analyzeDialGap(path, dur/2)
	}
}
