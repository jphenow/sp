package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jphenow/sp/internal/sprite"
)

func TestIsTransportFailure(t *testing.T) {
	// Left column: real messages seen from the sprite CLI when a connection
	// drops. Right column: failures that mean the session ended.
	transport := []string{
		"Error: connection closed",
		"Error: failed to start sprite command: failed to connect: read tcp 192.168.1.240:51196->169.155.48.226:443: i/o timeout",
		"curl: (35) Recv failure: Connection reset by peer",
		"websocket: close 1006 (abnormal closure)",
		"dial tcp: lookup api.sprites.dev: no such host",
		"Put \"https://api.sprites.dev/...\": net/http: request canceled (Client.Timeout exceeded while awaiting headers)",
	}
	notTransport := []string{
		"",
		"exit status 1",
		"Error: session 42 not found",
		"bash: command not found",
		"Error: unknown shorthand flag: 'c'",
	}
	for _, s := range transport {
		if !isTransportFailure(s) {
			t.Errorf("isTransportFailure(%q) = false, want true", s)
		}
	}
	for _, s := range notTransport {
		if isTransportFailure(s) {
			t.Errorf("isTransportFailure(%q) = true, want false", s)
		}
	}
}

// fakeClient writes a script that counts its invocations and behaves according
// to per-attempt instructions, standing in for `sprite attach`.
func fakeClient(t *testing.T, script string) (path, counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "count")
	path = filepath.Join(dir, "fake")
	body := "#!/bin/sh\nn=$(cat " + counter + " 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > " + counter + "\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, counter
}

func runs(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

func TestRunWithReconnect(t *testing.T) {
	sameTarget := func(args []string) func() *attachTarget {
		return func() *attachTarget { return &attachTarget{args: args, label: "test"} }
	}

	t.Run("clean exit is not retried", func(t *testing.T) {
		bin, counter := fakeClient(t, "exit 0")
		if err := runWithReconnect(bin, attachTarget{label: "test"}, sameTarget(nil)); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got := runs(t, counter); got != 1 {
			t.Errorf("ran %d times, want 1", got)
		}
	})

	t.Run("non-transport failure is not retried", func(t *testing.T) {
		bin, counter := fakeClient(t, "echo 'bash: no such file' >&2; exit 2")
		if err := runWithReconnect(bin, attachTarget{label: "test"}, sameTarget(nil)); err == nil {
			t.Fatal("want the command's error to propagate")
		}
		if got := runs(t, counter); got != 1 {
			t.Errorf("ran %d times, want 1", got)
		}
	})

	t.Run("dropped connection reconnects and succeeds", func(t *testing.T) {
		bin, counter := fakeClient(t, `if [ "$n" -lt 3 ]; then echo "Error: connection closed" >&2; exit 1; fi; exit 0`)
		if err := runWithReconnect(bin, attachTarget{label: "test"}, sameTarget(nil)); err != nil {
			t.Fatalf("err = %v, want nil after reconnect", err)
		}
		if got := runs(t, counter); got != 3 {
			t.Errorf("ran %d times, want 3", got)
		}
	})

	t.Run("stops after retrying a session the listing cannot find", func(t *testing.T) {
		bin, counter := fakeClient(t, "echo 'Error: connection closed' >&2; exit 1")
		// nil means the listing found no session. That is not proof the
		// session ended — the column it matches on is truncated — so the
		// original attach is retried a bounded number of times, then the loop
		// ends quietly rather than erroring.
		err := runWithReconnect(bin, attachTarget{label: "test"}, func() *attachTarget { return nil })
		if err != nil {
			t.Fatalf("err = %v, want nil once the retries are spent", err)
		}
		if got, want := runs(t, counter), 1+maxBlindReattach; got != want {
			t.Errorf("ran %d times, want %d", got, want)
		}
	})
}

// TestIsOneShotTmux covers the session rows that must never be attached to.
// A finished `tmux list-sessions` exec is listed just like an interactive
// session; attaching to it connects and paints nothing, which looks exactly
// like the hang this code is supposed to recover from.
func TestIsOneShotTmux(t *testing.T) {
	oneShot := []string{
		"tmux list-sessions",
		"tmux has-session -t bash",
		"sh -c for s in $(tmux list-sessions -F ...); do tmux setenv ...",
		"tmux kill-session -t bash",
		"tmux capture-pane -p",
	}
	for _, cmd := range oneShot {
		if !isOneShotTmux(cmd) {
			t.Errorf("isOneShotTmux(%q) = false, want true", cmd)
		}
	}

	interactive := []string{
		"tmux new-session -A -s bash",
		"tmux attach -t bash",
		"sh -c exec tmux new-session -A -s bash",
	}
	for _, cmd := range interactive {
		if isOneShotTmux(cmd) {
			t.Errorf("isOneShotTmux(%q) = true, want false", cmd)
		}
	}
}

// TestReconnectRefusalIsTransportFailure covers the message the sprite CLI
// emits when it declines to reconnect an exec with stdin attached. It names no
// socket error, so none of the other markers matched and sp ended the session
// instead of reattaching.
func TestReconnectRefusalIsTransportFailure(t *testing.T) {
	stderr := "Error: connection to exec session tun-TtCSy4H9KaOQUj04 was lost " +
		"while stdin was attached; refusing unsafe automatic reconnect because " +
		"input delivery is uncertain: exit status 1"
	if !isTransportFailure(stderr) {
		t.Fatal("the reconnect refusal should count as a transport failure")
	}
}

func TestLastVisibleLine(t *testing.T) {
	t.Run("finds the last line under a full-screen session's escape codes", func(t *testing.T) {
		output := "\x1b[?1049h\x1b[2J\x1b[H$ make build\r\n\x1b[32mok\x1b[m\r\n" +
			"\x1b[?1049lError: session not found\r\n"
		if got := lastVisibleLine(output); got != "Error: session not found" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("reports nothing when the client only moved the cursor", func(t *testing.T) {
		if got := lastVisibleLine("\x1b[?1049h\x1b[2J\x1b[H\x1b[?25l"); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("bounds a long line", func(t *testing.T) {
		got := lastVisibleLine(strings.Repeat("x", 500))
		if len(got) != 203 || !strings.HasSuffix(got, "...") {
			t.Fatalf("got %d chars: %q", len(got), got)
		}
	})
}

// TestReconnectWindowRestartsAfterALiveSession covers the budget that used to
// run from the moment sp started: a session that had been up for a day had
// none of it left when the connection finally dropped, so the first failure
// ended the session instead of being reconnected.
func TestReconnectWindowRestartsAfterALiveSession(t *testing.T) {
	defer func(window, live time.Duration) {
		reconnectWindow, reconnectLiveFor = window, live
	}(reconnectWindow, reconnectLiveFor)
	reconnectWindow, reconnectLiveFor = 100*time.Millisecond, 100*time.Millisecond

	// The first attempt lasts past reconnectLiveFor, so it counts as a
	// connection that worked and the window starts over; the second fails at
	// once and spends what is left of it.
	bin, counter := fakeClient(t, `if [ "$n" = 1 ]; then sleep 0.3; fi; echo "Error: connection closed" >&2; exit 1`)
	if err := runWithReconnect(bin, attachTarget{label: "test"}, func() *attachTarget { return nil }); err == nil {
		t.Fatal("want an error once the window is spent")
	}
	if got := runs(t, counter); got != 2 {
		t.Errorf("ran %d times, want 2 — the live attempt should have restored the window", got)
	}
}

// TestSpriteStillThere covers the one distinction that matters on the ssh
// path: a sprite that is really gone ends the loop, while a listing that could
// not be fetched does not — being offline is the case the loop exists for, and
// it fails the listing exactly like a destroyed sprite would.
func TestSpriteStillThere(t *testing.T) {
	target := &attachTarget{label: "live"}
	cases := []struct {
		name   string
		script string
		want   *attachTarget
	}{
		{"listed", `echo '{"sprites":[{"id":"1","name":"live"}]}'`, target},
		{"not listed", `echo '{"sprites":[{"id":"2","name":"other"}]}'`, nil},
		{"listing failed", `echo 'dial tcp: no route to host' >&2; exit 1`, target},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "sprite")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			got := spriteStillThere(sprite.NewClient(""), "live", target)()
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
