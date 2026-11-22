package hub

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCardRungIgnoresStaleReportWhenCold is the behaviour the whole page rests
// on: a cold machine has no processes, so a report claiming Claude was alive
// describes something that no longer exists.
func TestCardRungIgnoresStaleReportWhenCold(t *testing.T) {
	report := &Report{ClaudeOK: true, TmuxOK: true, ReportedAt: time.Now().Add(-2 * time.Hour)}

	cold := Card{Name: "a", Status: "cold", Report: report}
	if got := cold.Rung(); got != RungCold {
		t.Fatalf("cold sprite with a stale report: got %v, want %v", got, RungCold)
	}

	running := Card{Name: "a", Status: "running", Report: report}
	if got := running.Rung(); got != RungClaude {
		t.Fatalf("running sprite with a Claude report: got %v, want %v", got, RungClaude)
	}
}

// TestCardRungWithoutReport covers a running sprite that has no agent.
func TestCardRungWithoutReport(t *testing.T) {
	c := Card{Name: "a", Status: "running"}
	if got := c.Rung(); got != RungMachine {
		t.Fatalf("got %v, want %v", got, RungMachine)
	}
}

// TestReportRungOrder checks the ladder picks the highest reached step.
func TestReportRungOrder(t *testing.T) {
	cases := []struct {
		name string
		rep  Report
		want Rung
	}{
		{"bare", Report{}, RungMachine},
		{"tmux", Report{TmuxOK: true}, RungTmux},
		{"claude", Report{TmuxOK: true, ClaudeOK: true}, RungClaude},
		{"remote", Report{TmuxOK: true, ClaudeOK: true, RemoteOK: true}, RungRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.Rung(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidation guards the values that get interpolated into shell commands.
func TestValidation(t *testing.T) {
	if err := validateSessionID("a6a8d11d-9989-4c63-8727-60d24ee7ba5f"); err != nil {
		t.Fatalf("valid uuid rejected: %v", err)
	}
	for _, bad := range []string{"", "; rm -rf /", "../etc", "a b"} {
		if err := validateSessionID(bad); err == nil {
			t.Fatalf("session id %q should have been rejected", bad)
		}
	}

	if err := validatePath("/home/sprite/work"); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	for _, bad := range []string{"", "relative/path", "/tmp/x\nrm -rf /"} {
		if err := validatePath(bad); err == nil {
			t.Fatalf("path %q should have been rejected", bad)
		}
	}

	if err := validateURL("https://hub-org.sprites.app"); err != nil {
		t.Fatalf("valid url rejected: %v", err)
	}
	for _, bad := range []string{"http://hub", "https://hub' ; id", ""} {
		if err := validateURL(bad); err == nil {
			t.Fatalf("url %q should have been rejected", bad)
		}
	}
}

// TestShellQuote checks the quoting used for directories in remote commands.
func TestShellQuote(t *testing.T) {
	if got, want := shellQuote(`/home/it's`), `'/home/it'\''s'`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestJobQueueIsIdempotentPerKey covers the double-tap case: a phone on a bad
// connection must not start two Claudes.
func TestJobQueueIsIdempotentPerKey(t *testing.T) {
	q := newJobQueue()
	release := make(chan struct{})
	var runs int
	var mu sync.Mutex

	first, started := q.start("s:claude", func() error {
		mu.Lock()
		runs++
		mu.Unlock()
		<-release
		return nil
	})
	if !started {
		t.Fatal("first call should have started a job")
	}

	second, started := q.start("s:claude", func() error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	})
	if started {
		t.Fatal("second call should have joined the running job")
	}
	if second != first {
		t.Fatalf("expected the same job id, got %q and %q", first, second)
	}

	close(release)
	waitFor(t, func() bool {
		job, _ := q.get(first)
		return job.State == JobDone
	})

	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("the action ran %d times, want 1", runs)
	}
}

// TestJobQueueRecordsFailure checks errors survive for the page to poll.
func TestJobQueueRecordsFailure(t *testing.T) {
	q := newJobQueue()
	id, _ := q.start("s:wake", func() error { return os.ErrPermission })
	waitFor(t, func() bool {
		job, _ := q.get(id)
		return job.State == JobFailed
	})
	job, _ := q.get(id)
	if job.Error == "" {
		t.Fatal("expected the failure message to be recorded")
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// TestStoreRoundTrip covers saving, reading back and dropping a report.
func TestStoreRoundTrip(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	rep := &Report{
		Sprite:     "gh-owner-repo",
		TmuxOK:     true,
		ClaudeOK:   true,
		ClaudeDir:  "/home/sprite/repo",
		Sessions:   []Session{{ID: "abc", Summary: "hello"}},
		ReportedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := store.SaveReport(rep); err != nil {
		t.Fatalf("saving: %v", err)
	}

	got, err := store.Reports()
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	back, ok := got["gh-owner-repo"]
	if !ok {
		t.Fatal("report missing after save")
	}
	if !back.ClaudeOK || back.ClaudeDir != "/home/sprite/repo" || len(back.Sessions) != 1 {
		t.Fatalf("report did not round-trip: %+v", back)
	}

	if err := store.DropReport("gh-owner-repo"); err != nil {
		t.Fatalf("dropping: %v", err)
	}
	got, _ = store.Reports()
	if _, ok := got["gh-owner-repo"]; ok {
		t.Fatal("report survived the drop")
	}
}

// TestStoreWatched covers the watched set.
func TestStoreWatched(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	if err := store.SetWatched("a", true); err != nil {
		t.Fatalf("watching: %v", err)
	}
	// Repeat adds must not error — the page toggles this freely.
	if err := store.SetWatched("a", true); err != nil {
		t.Fatalf("re-watching: %v", err)
	}
	watched, _ := store.Watched()
	if !watched["a"] {
		t.Fatal("expected a to be watched")
	}

	if err := store.SetWatched("a", false); err != nil {
		t.Fatalf("unwatching: %v", err)
	}
	watched, _ = store.Watched()
	if watched["a"] {
		t.Fatal("expected a to be unwatched")
	}
}

// TestReadSessionHead parses the transcript shape Claude actually writes:
// metadata records first, then a meta caveat, then the real opening message.
func TestReadSessionHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := `{"type":"mode","mode":"normal","sessionId":"abc"}
{"type":"file-history-snapshot","messageId":"x"}
{"type":"user","message":{"role":"user","content":"caveat text"},"isMeta":true,"cwd":"/home/sprite/work"}
{"type":"user","message":{"role":"user","content":"  fix   the   build  "},"cwd":"/home/sprite/work"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	dir, summary := readSessionHead(path)
	if dir != "/home/sprite/work" {
		t.Fatalf("dir: got %q", dir)
	}
	if summary != "fix the build" {
		t.Fatalf("summary: got %q, want %q", summary, "fix the build")
	}
}

// TestReadSessionHeadBlockContent covers the array-of-blocks message shape.
func TestReadSessionHeadBlockContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":[{"type":"image"},{"type":"text","text":"look at this"}]},"cwd":"/x"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	_, summary := readSessionHead(path)
	if summary != "look at this" {
		t.Fatalf("got %q", summary)
	}
}

// TestTrimSummaryTruncates keeps the phone layout from being blown apart by a
// pasted wall of text.
func TestTrimSummaryTruncates(t *testing.T) {
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	got := trimSummary(string(long))
	if len([]rune(got)) > summaryLimit+1 {
		t.Fatalf("summary not truncated: %d runes", len([]rune(got)))
	}
}

// TestIsClaudeProcess checks process matching, including the case that
// motivated matching on the base name rather than a substring.
func TestIsClaudeProcess(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/home/sprite/.local/bin/claude"}, true},
		{[]string{"claude", "--continue"}, true},
		{[]string{"node", "/opt/claude/claude", "--resume"}, true},
		{[]string{"vim", "claude-notes.md"}, false},
		{[]string{"/usr/local/bin/sp", "hub", "agent"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isClaudeProcess(tc.args); got != tc.want {
			t.Fatalf("isClaudeProcess(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
