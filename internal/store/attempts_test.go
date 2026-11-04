package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAndReadAttempts(t *testing.T) {
	db, err := OpenPath(filepath.Join(t.TempDir(), "sp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	rows := []Attempt{
		{At: now.Add(-48 * time.Hour), Sprite: "old", Outcome: "clean"},
		{At: now.Add(-time.Hour), Sprite: "one", Transport: "ssh", Outcome: "dropped",
			Uptime: 90 * time.Second, FirstOutput: 11700 * time.Millisecond, Attempt: 2, Detail: "closed"},
		{At: now.Add(-time.Minute), Sprite: "two", Outcome: "stall"},
	}
	for _, a := range rows {
		if err := db.RecordAttempt(a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.Attempts(now.Add(-24*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d attempts, want 2 within the window", len(got))
	}
	// Newest first.
	if got[0].Sprite != "two" || got[1].Sprite != "one" {
		t.Errorf("got %q then %q, want \"two\" then \"one\"", got[0].Sprite, got[1].Sprite)
	}
	if got[1].Uptime != 90*time.Second || got[1].FirstOutput != 11700*time.Millisecond {
		t.Errorf("durations round-tripped as %v / %v", got[1].Uptime, got[1].FirstOutput)
	}
	if got[1].Transport != "ssh" || got[1].Outcome != "dropped" || got[1].Attempt != 2 || got[1].Detail != "closed" {
		t.Errorf("row round-tripped as %+v", got[1])
	}

	one, err := db.Attempts(now.Add(-24*time.Hour), "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Sprite != "one" {
		t.Errorf("got %+v, want only the \"one\" rows", one)
	}
}
