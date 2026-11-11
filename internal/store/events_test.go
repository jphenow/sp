package store

import (
	"testing"
	"time"
)

func TestRecordAndReadEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	for _, e := range []Event{
		{At: now.Add(-48 * time.Hour), Source: "sync", Sprite: "old", Message: "before the window"},
		{At: now.Add(-time.Hour), Source: "claude-config", Sprite: "blue", Message: "packing skills failed", Detail: "permission denied"},
		{At: now.Add(-time.Minute), Level: "error", Source: "sync", Sprite: "green", Message: "mutagen gone"},
	} {
		if err := db.RecordEvent(e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.Events(now.Add(-24*time.Hour), EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events in the window, want 2", len(got))
	}
	// Newest first, so the most recent problem is the one you read.
	if got[0].Message != "mutagen gone" {
		t.Errorf("first = %q, want the newest", got[0].Message)
	}
	// An unset level is a warning: everything sp prints as one arrives here
	// without saying so.
	if got[1].Level != "warn" {
		t.Errorf("level = %q, want warn by default", got[1].Level)
	}
	if got[1].Detail != "permission denied" {
		t.Errorf("detail = %q, want it kept", got[1].Detail)
	}

	for _, tc := range []struct {
		name   string
		filter EventFilter
		want   int
	}{
		{"by sprite", EventFilter{Sprite: "blue"}, 1},
		{"by level", EventFilter{Level: "error"}, 1},
		{"by source", EventFilter{Source: "sync"}, 1},
		{"limited", EventFilter{Limit: 1}, 1},
		{"nothing matches", EventFilter{Sprite: "absent"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Events(now.Add(-24*time.Hour), tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Errorf("got %d, want %d", len(got), tc.want)
			}
		})
	}
}

// TestPrune covers the retention sweep. Both tables only ever grow, and a
// prune that missed one of them would leave the database unbounded anyway.
func TestPrune(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	old, recent := now.Add(-40*24*time.Hour), now.Add(-time.Hour)
	for _, at := range []time.Time{old, recent} {
		if err := db.RecordEvent(Event{At: at, Source: "sync", Message: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordAttempt(Attempt{At: at, Sprite: "blue", Outcome: "dropped"}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.Prune(now.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d rows, want 2 (one from each table)", n)
	}

	events, err := db.Events(now.Add(-365*24*time.Hour), EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := db.Attempts(now.Add(-365*24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || len(attempts) != 1 {
		t.Errorf("kept %d events and %d attempts, want 1 of each", len(events), len(attempts))
	}
}
