package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jphenow/sp/internal/progress"
	"github.com/jphenow/sp/internal/store"
)

// TestWarningsAreKept is the point of the whole mechanism: a warning printed
// while a session is starting has to survive the session, because that is
// exactly when nobody can read it.
func TestWarningsAreKept(t *testing.T) {
	isolateStore(t)

	progress.WarnfFrom("claude-config", "Warning: packing %s: %v", "skills", os.ErrPermission)
	warnf("sync", "sync setup failed: %v", os.ErrDeadlineExceeded)

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.Events(time.Now().Add(-time.Minute), store.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d warnings, want 2", len(got))
	}

	bySource := map[string]store.Event{}
	for _, e := range got {
		bySource[e.Source] = e
	}
	if e, ok := bySource["claude-config"]; !ok {
		t.Error("a warning routed through the progress package lost its source")
	} else if !strings.Contains(e.Message, "packing skills") {
		t.Errorf("message = %q", e.Message)
	}
	if e, ok := bySource["sync"]; !ok {
		t.Error("a warning printed straight to stderr was not kept")
	} else if !strings.Contains(e.Message, "sync setup failed") {
		t.Errorf("message = %q", e.Message)
	}
}

// TestCountEventSources covers the tally, which is how a problem that keeps
// recurring is meant to be visible without reading every line.
func TestCountEventSources(t *testing.T) {
	got := countEventSources([]store.Event{
		{Source: "sync"},
		{Source: "claude-config"},
		{Source: "sync"},
		{Source: "hold"},
		{Source: "sync"},
		{Source: "claude-config"},
	})
	want := []sourceCount{{"sync", 3}, {"claude-config", 2}, {"hold", 1}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh.log")
	if err := os.WriteFile(path, []byte("one\n\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Blank lines are not an account of anything, and the end of the file is
	// the part that describes how the connection finished.
	if got, want := tailLines(path, 2), "three\nfour"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := tailLines(filepath.Join(t.TempDir(), "absent.log"), 2); got != "" {
		t.Errorf("a missing log should read as nothing, got %q", got)
	}
	if got := tailLines("", 2); got != "" {
		t.Errorf("a client with no log should read as nothing, got %q", got)
	}
}
