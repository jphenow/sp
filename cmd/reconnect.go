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
	// The ssh transport has a vocabulary of its own, and none of the above is
	// in it. "Timeout, server ... not responding" is ssh's own verdict from
	// ServerAliveInterval, which sp configures precisely so the client decides
	// before the stall detector has to guess — a verdict it was then throwing
	// away.
	"closed by remote host",
	"timeout, server",
}

// "Connection to <host> closed." is deliberately NOT treated as a dropped
// connection, though it is the line that appears after most of them.
//
// It was, briefly, and that was a mistake worth leaving a note about. ssh
// prints it whenever the remote command ends, for any reason, and the thing
// that says which reason is the exit status: 255 is ssh's own verdict that the
// transport failed, and anything else is the remote command's own status,
// forwarded. The logs are explicit about the difference --
//
//	client_input_channel_req: channel 0 rtype exit-status reply 0
//	Exit status 1          <- the remote command exited; the connection was fine
//	Exit status -1         <- no status arrived; ssh gave up on the transport
//
// -- so matching the message overrode the one signal that discriminates, and
// turned a remote command that exits immediately into an unbounded retry loop.
// isTransportExit against 255 is the correct test and was already there.

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

// isSignalDeath reports whether the client was killed by a signal instead of
// exiting on its own.
//
// A signal is not the client's verdict on the session — the process never got
// to form one. sp used to read one as a deliberate exit and hand the terminal
// back, which is how an ssh client killed by something outside sp ended a
// session that was still perfectly alive on the sprite. With nothing attached
// to hold it awake the sprite then idle-paused, stopped half an hour later,
// and took the tmux server and every pane in it along.
//
// Interrupts are excluded. SIGINT and SIGQUIT are how a person says stop, and
// retrying after one would leave no way to end a session by hand.
func isSignalDeath(err error) bool {
	sig, ok := deathSignal(err)
	if !ok {
		return false
	}
	switch sig {
	case syscall.SIGINT, syscall.SIGQUIT:
		return false
	}
	return true
}

// deathSignal returns the signal the client died on.
func deathSignal(err error) (syscall.Signal, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return status.Signal(), true
}

// signalName describes the signal a client died on, or "" if it exited on its
// own.
//
// Worth recording rather than folding into "killed", because the signal is
// most of the diagnosis and the three common ones mean entirely different
// things: a hangup says the terminal or parent went away, a termination says
// something asked politely and can be traced to whatever asks, and a kill says
// the kernel or a -9 — out of memory, or deliberate. Both the number and the
// name go in, since the name alone ("killed") is the same word sp uses for the
// outcome and says nothing extra.
func signalName(err error) string {
	sig, ok := deathSignal(err)
	if !ok {
		return ""
	}
	return fmt.Sprintf("signal %d (%v)", int(sig), sig)
}

// tailLines returns the last n non-empty lines of a file, or "" if it cannot
// be read. Bounded: a verbose transport log of a long session is large, and
// only its end describes how the session finished.
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	const window = 8192
	start := info.Size() - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return ""
	}

	lines := strings.Split(string(buf), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			kept = append([]string{l}, kept...)
		}
	}
	return strings.Join(kept, "\n")
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
	// transportLog is a file the client writes its own account of the
	// connection to, or "" when it has none. On a drop this is the only
	// explanation that exists: what reached the terminal is the session's own
	// output, not the transport's reasoning.
	transportLog string
}

// renewSessionHold refreshes the keepalive hold on the sprite, or is nil when
// this session holds nothing (--no-hold, --keep-warm, or a command with no
// session to hold).
//
// The hold is capped so a session left open forever cannot keep a sprite
// Active forever, but nothing ever renewed it, so the cap expired underneath
// sessions that were still in use. After it did, a connection that dropped
// left the sprite with no reason to stay awake: it idle-paused, stopped soon
// after, and took the tmux server and every pane in it along. Renewing on each
// reattach measures the cap from the last time sp was actually there, which is
// what it was always meant to mean.
var renewSessionHold func()

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
		// The first attempt was launched with a fresh hold; every one after it
		// is a reattach, and restores the hold the drop may have outlived.
		if attempt > 1 && renewSessionHold != nil {
			renewSessionHold()
		}

		tail := &tailWriter{}
		// The client runs on a pty sp owns rather than on the real terminal,
		// so sp can see the stream and notice a connection that has stopped
		// answering. tail gets a copy for classifying the failure.
		started := time.Now()
		outcome, err := runProxied(binary, target.args, tail, pump)
		resetTerminalForRetry()
		uptime := time.Since(started)
		dropped := outcome.stalled ||
			(err != nil && (isTransportFailure(tail.String()) ||
				isTransportExit(err, target.transportExit) ||
				isSignalDeath(err)))
		detail := lastVisibleLine(tail.String())
		if sig := signalName(err); sig != "" {
			detail = strings.TrimSpace(sig + " " + detail)
		}
		recordAttempt(store.Attempt{
			Sprite:      target.label,
			Transport:   filepath.Base(binary),
			Outcome:     attemptName(err, outcome.stalled, dropped),
			Uptime:      uptime,
			FirstOutput: outcome.firstOutput,
			Attempt:     attempt,
			Detail:      detail,
		})
		// The transport's own log, kept for anything that was not a clean
		// exit. This used to be kept only for a classified drop, which had it
		// backwards: an attempt sp could not classify is the one most in need
		// of an explanation, and it was the only one that recorded none. Three
		// sessions died that way with nothing left behind but the exit status,
		// because the next session truncated the log before anyone read it.
		// A clean exit still records nothing -- there is nothing to explain,
		// and keeping every one of those would bury the rest.
		if err != nil {
			// What the session printed as it went comes first: a remote
			// command that exited on its own usually said why, and that
			// sentence is the whole answer. The transport's log follows, for
			// the case where nothing was said because nothing got through.
			var parts []string
			if said := lastVisibleLines(tail.String(), 5); said != "" {
				parts = append(parts, "session output:\n"+said)
			}
			if tailed := tailLines(target.transportLog, 8); tailed != "" {
				parts = append(parts, "transport log:\n"+tailed)
			}
			if len(parts) > 0 {
				recordEvent(store.Event{Level: "warn", Source: "transport",
					Sprite: target.label,
					Message: fmt.Sprintf("session on %s ended after %s as %q (%v)",
						target.label, uptime.Round(time.Second),
						attemptName(err, outcome.stalled, dropped), err),
					Detail: strings.Join(parts, "\n\n")})
			}
		}

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
	case isSignalDeath(err):
		return "killed"
	case dropped:
		return "dropped"
	default:
		return "ended"
	}
}

// recordAttempt appends one ended attempt to sp's local log.
//
// This exists to answer "is this sprite really worse, or does it only feel
// worse". The database is opened per attempt rather than held, because an
// attempt can last days and the daemon writes to the same file.
func recordAttempt(a store.Attempt) {
	db, err := store.Open()
	if err != nil {
		warnAttemptLog(err)
		return
	}
	defer db.Close()
	if err := db.RecordAttempt(a); err != nil {
		warnAttemptLog(err)
	}
}

// attemptLogWarned keeps a broken attempt log to one warning per run, so a
// database that cannot be written does not print on every reconnect.
var attemptLogWarned sync.Once

// warnAttemptLog reports that an attempt went unrecorded.
//
// This was a debug line, on the grounds that a question about reliability is
// not worth interrupting a reconnect over. That was the wrong trade: the log is
// the only answer to "does this sprite really drop more often", and failing
// quietly means the answer comes back missing rather than known to be absent.
func warnAttemptLog(err error) {
	slog.Debug("recording session attempt", "err", err)
	attemptLogWarned.Do(func() {
		fmt.Fprintf(os.Stderr,
			"Note: sp could not record this session attempt (%v); `sp disconnects` will be incomplete.\n", err)
	})
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

// lastVisibleLines is lastVisibleLine's neighbours as well, oldest first.
//
// One line is the right size for telling the user why their session ended, but
// the wrong size for working out why afterwards: ssh's parting message lands
// last and buries whatever the session itself said on the way out. tmux
// explaining that it lost its server is two lines above the line sp kept.
func lastVisibleLines(output string, n int) string {
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

	var kept []string
	lines := strings.FieldsFunc(cleaned, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			kept = append([]string{line}, kept...)
		}
	}
	return strings.Join(kept, "\n")
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
