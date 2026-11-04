package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/jphenow/sp/internal/store"
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
	// reconnectMaxBackoff caps the wait between attempts. A reconnect is a
	// dial and nothing more, so retrying every minute for hours is cheap,
	// and it bounds how long a laptop that just came back online spends
	// waiting for sp to notice.
	reconnectMaxBackoff = time.Minute
	// stderrTailBytes is how much of the child's stderr is kept to classify a
	// failure. The messages we match are short and come last.
	stderrTailBytes = 4096
	// maxBlindReattach is how many times to re-run the original attach when
	// the session listing reports nothing. Bounded, so a session that really
	// has ended stops the loop instead of being recreated forever.
	maxBlindReattach = 2
)

// Both of these are variables only so tests can shrink them.
var (
	// reconnectWindow bounds an *unbroken run* of failures before sp gives up
	// and hands the terminal back. It is not a budget for the session: a
	// session is meant to outlive the laptop it was opened from, so every
	// attempt that lasted long enough to have been a working connection
	// starts the window over.
	//
	// Long, because the thing on the other end is persistent. The sprite and
	// its tmux session survive a night of being closed, a flight, a move
	// between networks; the only reason to stop trying is that nobody is
	// coming back. Reconnecting after hours away is the normal case, not an
	// edge one, and the alternative to waiting is a session the user has to
	// reattach by hand for no reason.
	reconnectWindow = 12 * time.Hour
	// reconnectLiveFor is how long an attempt has to last before it counts as
	// having really connected, which is what resets the window.
	//
	// It cannot be short. A dial that fails while the network is away can
	// itself take tens of seconds — the ssh client waits out its connect
	// timeout — and counting one of those as a live session would make the
	// window reset forever and the loop never end.
	reconnectLiveFor = 5 * time.Minute
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
	// The sprite CLI refuses to reconnect an exec by itself when stdin is
	// attached, because it cannot prove keystrokes were delivered. For sp that
	// refusal is still a dropped connection and still worth retrying: sp
	// attaches to a tmux session, so the session state lives on the sprite and
	// a fresh attach picks it up intact — the uncertain input the CLI is
	// protecting is a keystroke in flight, not the work.
	"was lost while stdin was attached",
	"refusing unsafe automatic reconnect",
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

// isTransportExit reports whether the client exited with the status it uses to
// mean the transport failed.
func isTransportExit(err error, status int) bool {
	if err == nil || status == 0 {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}

	return exitErr.ExitCode() == status
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
	// transportExit is an exit status this client uses to mean "the transport
	// failed", or 0 when it has none. ssh reports 255, which is a far better
	// signal than reading its stderr: it is the client telling us plainly that
	// the connection never carried the session, rather than a message we have
	// to recognize.
	transportExit int
}

// runWithReconnect runs an interactive sprite client command, re-running it
// when the connection drops.
//
// nextAttach is called before every attempt after the first; returning nil
// means the session no longer exists, which ends the loop without an error —
// the session is genuinely over, not unreachable.
func runWithReconnect(binary string, first attachTarget, nextAttach func() *attachTarget) error {
	// Leaving the alternate screen belongs to handing the terminal back, not to
	// the gap between two attaches. It also has to happen *before* the message
	// explaining why sp is handing it back: anything printed on the alternate
	// screen is erased on the way out, which is how a session could end with no
	// visible reason at all. The deferred call is the catch-all; the handback
	// paths below reset first and then print. Resetting twice is harmless.
	defer resetTerminal()

	// One reader for the terminal, shared by every attempt.
	pump := newStdinPump()

	target := first
	failingSince := time.Now()
	backoff := time.Second
	blind := 0

	for attempt := 1; ; attempt++ {
		tail := &tailWriter{}
		// The client runs on a pty sp owns rather than on the real terminal,
		// so sp can see the stream and notice a connection that has stopped
		// answering. tail gets a copy for classifying the failure.
		started := time.Now()
		outcome, err := runProxied(binary, target.args, tail, pump)
		resetTerminalForRetry()
		uptime := time.Since(started)
		dropped := outcome.stalled || (err != nil && (isTransportFailure(tail.String()) || isTransportExit(err, target.transportExit)))
		recordAttempt(store.Attempt{
			Sprite:      target.label,
			Transport:   filepath.Base(binary),
			Outcome:     attemptName(err, outcome.stalled, dropped),
			Uptime:      uptime,
			FirstOutput: outcome.firstOutput,
			Attempt:     attempt,
			Detail:      lastVisibleLine(tail.String()),
		})

		// A stall is a dropped connection that nothing reported, so it is
		// retried like any other — the tmux session on the sprite is untouched
		// and a fresh attach picks it up.
		if !dropped {
			resetTerminal()
			explainExit(target.label, err, tail.String())
			return err
		}
		if outcome.stalled {
			fmt.Fprintf(os.Stderr,
				"\nConnection to %s stopped responding (no reply for %s).\n",
				target.label, stallEchoGrace)
		}
		// An attempt that stayed up this long was a working connection, so
		// whatever went wrong afterwards is a new problem and gets the whole
		// window to itself. Without this the window ran from the moment sp
		// started, which meant a session that had been fine for a day had no
		// budget left at all when it finally dropped.
		if uptime >= reconnectLiveFor {
			failingSince, backoff = time.Now(), time.Second
		}
		if time.Since(failingSince) >= reconnectWindow {
			resetTerminal()
			return fmt.Errorf("connection to %s lost and not restored after %s: %w",
				target.label, time.Since(failingSince).Round(time.Second), err)
		}

		fmt.Fprintf(os.Stderr, "\nConnection lost (%v). Reconnecting in %s — Ctrl-C to stop.\n",
			err, backoff.Round(time.Second))
		if interrupted := sleepInterruptible(backoff); interrupted {
			return fmt.Errorf("reconnect to %s cancelled", target.label)
		}
		if backoff *= 2; backoff > reconnectMaxBackoff {
			backoff = reconnectMaxBackoff
		}

		// An empty listing is not proof the session ended: it only matches a
		// session whose command column still reads "tmux", and that column is
		// truncated. So the original attach is retried a bounded number of
		// times first. It ends in `tmux new-session -A`, which attaches to the
		// session when it exists and creates it when it does not, so it is the
		// right command either way. A deliberate exit never reaches here —
		// that exits cleanly and returns above.
		if next := nextAttach(); next != nil {
			target, blind = *next, 0
		} else if blind++; blind <= maxBlindReattach {
			target = first
		} else {
			resetTerminal()
			fmt.Fprintf(os.Stderr, "Session on %s is gone; not reconnecting.\n", target.label)
			return nil
		}
		fmt.Fprintf(os.Stderr, "Reconnecting to %s (attempt %d)...\n", target.label, attempt+1)
	}
}

// attemptName labels how an attempt ended, for the recorded log.
func attemptName(err error, stalled, dropped bool) string {
	switch {
	case err == nil:
		return "clean"
	case stalled:
		return "stall"
	case dropped:
		return "dropped"
	default:
		return "ended"
	}
}

// recordAttempt appends one ended attempt to sp's local log.
//
// Best-effort and silent: this exists to answer "is this sprite really worse,
// or does it only feel worse", and a question about reliability is not worth
// interrupting a reconnect over. The database is opened per attempt rather
// than held, because an attempt can last days and the daemon writes to the
// same file.
func recordAttempt(a store.Attempt) {
	db, err := store.Open()
	if err != nil {
		slog.Debug("recording session attempt", "err", err)
		return
	}
	defer db.Close()
	if err := db.RecordAttempt(a); err != nil {
		slog.Debug("recording session attempt", "err", err)
	}
}

// explainExit reports why the session ended when the client exited non-zero
// and sp decided that was not a dropped connection.
//
// Without this the only thing left on screen is the exit status, which looks
// identical whether the client said something sp did not recognize as a
// connection failure or said nothing at all. Printing its last line makes the
// difference visible — and an empty one is itself the useful answer.
func explainExit(label string, err error, output string) {
	if err == nil {
		return
	}
	if line := lastVisibleLine(output); line != "" {
		fmt.Fprintf(os.Stderr, "\nSession on %s ended (%v): %s\n", label, err, line)
		return
	}
	fmt.Fprintf(os.Stderr,
		"\nSession on %s ended (%v) without printing a reason, so sp could not tell it from a deliberate exit.\n",
		label, err)
}

// terminalEscape matches the CSI and OSC sequences a full-screen session leaves
// all over the captured output.
var terminalEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>]`)

// lastVisibleLine returns the final line of real text in captured terminal
// output, bounded for display. The capture is raw pty bytes, so escape
// sequences and control characters are stripped first.
func lastVisibleLine(output string) string {
	cleaned := terminalEscape.ReplaceAllString(output, "")
	cleaned = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, cleaned)

	lines := strings.FieldsFunc(cleaned, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			return line
		}
	}
	return ""
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
