package sprite

import (
	"strings"
	"testing"
	"time"
)

// The trace prints to stderr, and several exec scripts embed credentials
// directly in the command string. A leak here would write an OAuth refresh
// token into the user's terminal and scrollback, so redaction is the one
// property of this package worth testing hard.
func TestDescribeExecRedactsSecrets(t *testing.T) {
	creds := "eyJjbGF1ZGVBaU9hdXRoIjp7ImFjY2Vzc1Rva2VuIjoic2stYW50LW9hdDAxLXNlY3JldCJ9fQ=="
	opts := ExecOptions{
		Sprite:  "my-sprite",
		Command: []string{"sh", "-c", "printf '%s' '" + creds + "' | base64 -d > ~/.claude/.credentials.json"},
	}
	got := describeExec(opts)
	if strings.Contains(got, creds) {
		t.Fatalf("credential blob survived redaction: %q", got)
	}
	if strings.Contains(got, "sk-ant-oat01") {
		t.Fatalf("token-shaped text survived redaction: %q", got)
	}
	if !strings.Contains(got, "my-sprite") {
		t.Errorf("sprite name should survive, got %q", got)
	}
}

func TestDescribeExecExcludesEnv(t *testing.T) {
	opts := ExecOptions{
		Sprite:  "s",
		Env:     map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-supersecret", "GH_TOKEN": "gho_secret"},
		Command: []string{"echo", "hi"},
	}
	got := describeExec(opts)
	for _, secret := range []string{"supersecret", "gho_secret", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if strings.Contains(got, secret) {
			t.Errorf("env leaked into trace detail (%q): %q", secret, got)
		}
	}
}

func TestDescribeExecUsesFirstRealLineAndUploads(t *testing.T) {
	opts := ExecOptions{
		Sprite:  "s",
		Command: []string{"sh", "-c", "\n\nmkdir -p ~/.claude\nchmod 700 ~/.claude\n"},
		Files:   map[string]string{"/local/.tmux.conf": "/home/sprite/.tmux.conf"},
	}
	got := describeExec(opts)
	if !strings.Contains(got, "mkdir -p ~/.claude") {
		t.Errorf("expected first non-empty script line, got %q", got)
	}
	if strings.Contains(got, "chmod 700") {
		t.Errorf("expected only the first line, got %q", got)
	}
	if !strings.Contains(got, ".tmux.conf") {
		t.Errorf("expected upload destination, got %q", got)
	}
	if strings.Contains(got, "/home/sprite/") {
		t.Errorf("upload label should use basenames, not full paths: %q", got)
	}
}

func TestTraceSummaryCountsAndOrders(t *testing.T) {
	ResetTrace()
	t.Cleanup(ResetTrace)

	now := time.Now()
	record("exec", "fast call", now.Add(-10*time.Millisecond), nil)
	record("exec", "slow cold wake", now.Add(-30*time.Second), nil)
	record("api", "info", now.Add(-5*time.Millisecond), nil)

	calls, busy := TotalTraced()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if busy < 30*time.Second {
		t.Errorf("busy = %s, want >= 30s", busy)
	}

	summary := TraceSummary()
	slow := strings.Index(summary, "slow cold wake")
	fast := strings.Index(summary, "fast call")
	if slow == -1 || fast == -1 {
		t.Fatalf("both calls should appear:\n%s", summary)
	}
	if slow > fast {
		t.Errorf("slowest call should be listed first:\n%s", summary)
	}
}

func TestTraceSummaryEmpty(t *testing.T) {
	ResetTrace()
	t.Cleanup(ResetTrace)
	if got := TraceSummary(); got != "" {
		t.Errorf("want empty summary with no spans, got %q", got)
	}
}

func TestDescribeExecCapsUploadList(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files["/local/"+n] = "/home/sprite/" + n
	}
	got := describeExec(ExecOptions{Sprite: "s", Command: []string{"true"}, Files: files})
	if !strings.Contains(got, "+3 more") {
		t.Errorf("expected the destination list to be capped, got %q", got)
	}
	if len(got) > 120 {
		t.Errorf("label should stay short for the in-flight footer, got %d chars: %q", len(got), got)
	}
}
