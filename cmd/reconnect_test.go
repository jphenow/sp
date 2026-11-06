package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jphenow/sp/internal/sprite"
	"github.com/jphenow/sp/internal/store"
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
		// The ssh transport's own messages. The first two cost a 5h session
		// that sp called a deliberate exit and refused to reattach; the third
		// is ssh's ServerAliveInterval verdict, which sp configures and so has
		// no excuse for not recognizing.
		"Connection to sprites.dev closed.",
		"Connection to sprites.dev closed by remote host.",
		"Timeout, server sprites.dev not responding.",
		"client_loop: send disconnect: Broken pipe",
	}
	notTransport := []string{
		"",
		"exit status 1",
		"Error: session 42 not found",
		"bash: command not found",
		"Error: unknown shorthand flag: 'c'",
		// Close to ssh's parting message without being it. A host name is
		// required, so ordinary session text that happens to talk about a
		// connection closing is not mistaken for the transport giving up.
		"the connection to the database was closed",
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

// isolateStore points the attempt log at a throwaway home, so running the
// tests does not append test sessions to the real one.
func isolateStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
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
	isolateStore(t)
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

	t.Run("client killed by a signal reconnects", func(t *testing.T) {
		// Nothing on stderr and no transport exit status: the only evidence is
		// that the process died on a signal. sp used to read this as the
		// session ending and hand the terminal back, while the tmux session it
		// was attached to was still running on the sprite.
		bin, counter := fakeClient(t, `if [ "$n" -lt 2 ]; then kill -9 $$; fi; exit 0`)
		if err := runWithReconnect(bin, attachTarget{label: "test"}, sameTarget(nil)); err != nil {
			t.Fatalf("err = %v, want nil after reconnect", err)
		}
		if got := runs(t, counter); got != 2 {
			t.Errorf("ran %d times, want 2", got)
		}
	})

	t.Run("interrupt is not retried", func(t *testing.T) {
		// Ctrl-C has to be able to end a session, so an interrupt is the one
		// signal that still counts as the client being told to stop.
		bin, counter := fakeClient(t, `kill -INT $$`)
		if err := runWithReconnect(bin, attachTarget{label: "test"}, sameTarget(nil)); err == nil {
			t.Fatal("want the interrupt to propagate")
		}
		if got := runs(t, counter); got != 1 {
			t.Errorf("ran %d times, want 1", got)
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
	isolateStore(t)
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

// TestSignalName covers the field that turns "killed" into a diagnosis. The
// three common signals mean entirely different things, and the record used to
// throw the distinction away.
func TestSignalName(t *testing.T) {
	for _, tc := range []struct {
		signal syscall.Signal
		want   string
	}{
		{syscall.SIGKILL, "signal 9 (killed)"},
		{syscall.SIGTERM, "signal 15 (terminated)"},
		{syscall.SIGHUP, "signal 1 (hangup)"},
	} {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Process.Signal(tc.signal); err != nil {
			t.Fatal(err)
		}
		if got := signalName(cmd.Wait()); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}

	// A client that exited on its own has no signal to report.
	if got := signalName(exec.Command("false").Run()); got != "" {
		t.Errorf("got %q, want empty for a plain non-zero exit", got)
	}
	if got := signalName(nil); got != "" {
		t.Errorf("got %q, want empty for a clean exit", got)
	}
}

// TestReconnectRenewsTheSessionHold covers the hold being restored on each
// reattach. Without it the hold's cap expired while a session was still in
// use, and the sprite was then free to pause and stop between two attempts.
func TestReconnectRenewsTheSessionHold(t *testing.T) {
	isolateStore(t)
	defer func(f func()) { renewSessionHold = f }(renewSessionHold)
	renewals := 0
	renewSessionHold = func() { renewals++ }

	bin, counter := fakeClient(t, `if [ "$n" = 1 ]; then echo "Error: connection closed" >&2; exit 1; fi; exit 0`)
	err := runWithReconnect(bin, attachTarget{label: "test"}, func() *attachTarget {
		return &attachTarget{label: "test"}
	})
	if err != nil {
		t.Fatalf("err = %v, want nil after reconnect", err)
	}
	if got := runs(t, counter); got != 2 {
		t.Fatalf("ran %d times, want 2", got)
	}
	// Once, for the reattach. Not for the first attempt: its caller had just
	// started a fresh hold.
	if renewals != 1 {
		t.Errorf("renewed the hold %d times, want 1", renewals)
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

// TestTransportLogIsKeptForAnUnclassifiedEnd covers the hole that cost three
// sessions' worth of evidence: the transport's log was recorded only when sp
// had already decided the attempt was a drop, so an attempt it could not
// classify -- the only kind anyone needs to investigate -- left nothing behind.
func TestTransportLogIsKeptForAnUnclassifiedEnd(t *testing.T) {
	isolateStore(t)

	logPath := filepath.Join(t.TempDir(), "ssh.log")
	if err := os.WriteFile(logPath, []byte("debug1: the transport's account\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exits non-zero saying nothing sp recognizes, which is the case that used
	// to be classified "ended" and recorded no explanation.
	bin, _ := fakeClient(t, "echo 'something unfamiliar' >&2\nexit 3")
	target := attachTarget{label: "test", transportLog: logPath}
	if err := runWithReconnect(bin, target, func() *attachTarget { return nil }); err == nil {
		t.Fatal("expected the non-zero exit to be returned")
	}

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	events, err := db.Events(time.Now().Add(-time.Minute), store.EventFilter{Source: "transport"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d transport events, want 1", len(events))
	}
	if !strings.Contains(events[0].Detail, "the transport's account") {
		t.Errorf("detail does not hold the transport log: %q", events[0].Detail)
	}
	if !strings.Contains(events[0].Message, `"ended"`) {
		t.Errorf("message does not say how the attempt was classified: %q", events[0].Message)
	}
}
