package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Dropping the connection to a sprite used to end the whole session: the local
// client exited with "Error: connection closed" and sp returned exit 1, even
// though the tmux session (and everything running in it) was still alive on the
// sprite. Closing a laptop, changing networks, or a few seconds of packet loss
// were enough.
//
// The sprite CLI reconnects in its `exec` path, holding keystrokes while it
// retries, but sp reattaches with `sprite attach`, which has no such logic and
// no flag to reach it. So sp retries the attach itself.
//
// Only connection failures are retried. A clean detach exits 0, and a session
// that really ended is left alone, so this never fights a deliberate exit.

const (
	// reconnectWindow bounds total time spent reconnecting before giving up
	// and handing the terminal back.
	reconnectWindow = 15 * time.Minute
	// reconnectMaxBackoff caps the wait between attempts.
	reconnectMaxBackoff = 30 * time.Second
	// stderrTailBytes is how much of the child's stderr is kept to classify a
	// failure. The messages we match are short and come last.
	stderrTailBytes = 4096
)

// transportErrorMarkers are substrings that identify a lost or refused
// connection, as opposed to a command that exited non-zero. Taken from the
// messages the sprite CLI and its websocket/HTTP layers actually emit.
var transportErrorMarkers = []string{
	"connection closed",
	"connection reset",
	"connection refused",
	"failed to connect",
	"i/o timeout",
	"broken pipe",
	"unexpected eof",
	"websocket: close",
	"no such host",
	"network is unreachable",
	"host is down",
	"tls handshake",
	"client.timeout exceeded",
	"context deadline exceeded",
}

// isTransportFailure reports whether captured stderr indicates a dropped
// connection rather than the session ending on its own.
func isTransportFailure(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, m := range transportErrorMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// tailWriter keeps the last stderrTailBytes written to it while passing
// everything through, so the child's stderr still reaches the terminal.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends to the tail buffer, trimming it to the configured size.
func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTailBytes {
		t.buf = t.buf[len(t.buf)-stderrTailBytes:]
	}
	return len(p), nil
}

// String returns the captured tail.
func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// attachTarget describes one attempt to attach: the argv to run and a label for
// messages. nextAttach re-resolves it each time, because after a drop the
// session may have a different id, or may be gone entirely.
type attachTarget struct {
	args  []string
	label string
}

// runWithReconnect runs an interactive sprite client command, re-running it
// when the connection drops.
//
// nextAttach is called before every attempt after the first; returning nil
// means the session no longer exists, which ends the loop without an error —
// the session is genuinely over, not unreachable.
func runWithReconnect(binary string, first attachTarget, nextAttach func() *attachTarget) error {
	target := first
	deadline := time.Now().Add(reconnectWindow)
	backoff := time.Second

	for attempt := 1; ; attempt++ {
		tail := &tailWriter{}
		cmd := exec.Command(binary, target.args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		// Tee stderr so the failure is both visible and classifiable. Only
		// stderr is wrapped; stdin and stdout stay attached to the real
		// terminal, which the client needs for raw mode and rendering.
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)

		err := cmd.Run()
		resetTerminal()

		if err == nil || !isTransportFailure(tail.String()) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connection to %s lost and not restored after %s: %w",
				target.label, reconnectWindow, err)
		}

		fmt.Fprintf(os.Stderr, "\nConnection lost (%v). Reconnecting in %s — Ctrl-C to stop.\n",
			err, backoff.Round(time.Second))
		if interrupted := sleepInterruptible(backoff); interrupted {
			return fmt.Errorf("reconnect to %s cancelled", target.label)
		}
		if backoff *= 2; backoff > reconnectMaxBackoff {
			backoff = reconnectMaxBackoff
		}

		next := nextAttach()
		if next == nil {
			fmt.Fprintf(os.Stderr, "Session on %s is gone; not reconnecting.\n", target.label)
			return nil
		}
		target = *next
		fmt.Fprintf(os.Stderr, "Reconnecting to %s (attempt %d)...\n", target.label, attempt+1)
	}
}

// sleepInterruptible waits for d, returning true if the user interrupted.
//
// Ctrl-C only reaches sp between attempts: while the client is running it owns
// the terminal and receives signals itself.
func sleepInterruptible(d time.Duration) bool {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		return true
	case <-time.After(d):
		return false
	}
}
