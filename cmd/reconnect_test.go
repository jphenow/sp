package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

// TestSprites2ReconnectRefusalIsTransportFailure covers the message Sprites 2
// emits when it declines to reconnect an exec with stdin attached. It names no
// socket error, so none of the older markers matched and sp ended the session
// instead of reattaching.
func TestSprites2ReconnectRefusalIsTransportFailure(t *testing.T) {
	stderr := "Error: connection to exec session tun-TtCSy4H9KaOQUj04 was lost " +
		"while stdin was attached; refusing unsafe automatic reconnect because " +
		"input delivery is uncertain: exit status 1"
	if !isTransportFailure(stderr) {
		t.Fatal("the Sprites 2 reconnect refusal should count as a transport failure")
	}
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
