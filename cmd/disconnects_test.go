package cmd

import (
	"testing"
	"time"

	"github.com/jphenow/sp/internal/store"
)

func TestSummarizeAttempts(t *testing.T) {
	attempts := []store.Attempt{
		{Sprite: "flaky", Outcome: "dropped", Uptime: time.Minute, FirstOutput: 12 * time.Second},
		{Sprite: "flaky", Outcome: "stall", Uptime: 2 * time.Minute, FirstOutput: 11 * time.Second},
		// Never printed anything, so it never connected: it must not pull the
		// median wake down to zero.
		{Sprite: "flaky", Outcome: "dropped", Uptime: 3 * time.Minute},
		{Sprite: "steady", Outcome: "clean", Uptime: 4 * time.Hour, FirstOutput: 300 * time.Millisecond},
	}

	got := summarizeAttempts(attempts)
	if len(got) != 2 {
		t.Fatalf("got %d sprites, want 2", len(got))
	}
	// Busiest first.
	if got[0].sprite != "flaky" {
		t.Errorf("got %q first, want \"flaky\"", got[0].sprite)
	}
	flaky := got[0]
	if flaky.total != 3 || flaky.outcomes["dropped"] != 2 || flaky.outcomes["stall"] != 1 {
		t.Errorf("flaky summarized as %+v", flaky)
	}
	if flaky.medianUptime != 2*time.Minute || flaky.longestUptime != 3*time.Minute {
		t.Errorf("uptimes: median %v, longest %v", flaky.medianUptime, flaky.longestUptime)
	}
	if flaky.medianWake != 12*time.Second {
		t.Errorf("median wake = %v, want 12s over the two attempts that printed", flaky.medianWake)
	}
	if got[1].medianWake != 300*time.Millisecond {
		t.Errorf("steady median wake = %v", got[1].medianWake)
	}
}

func TestShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                            "-",
		11700 * time.Millisecond:     "11.7s",
		90 * time.Second:             "1m30s",
		3*time.Hour + 11*time.Minute: "3h11m",
		49 * time.Hour:               "2d01h",
	}
	for d, want := range cases {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
