package cmd

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// A sprite connection can die without either end noticing. The TCP session goes
// half-open — a laptop sleeps, a NAT drops the mapping, a network changes — and
// the client sits in a read that will never return. Nothing times out, so the
// reconnect logic never runs: it only sees failures the client reports, and a
// client that is waiting forever reports nothing. Ctrl-C does not help either,
// because the client owns the terminal in raw mode and receives the keystroke
// itself. The session has to be killed from a second terminal.
//
// What makes this detectable is that a hung connection and an idle one are not
// actually alike. Both are silent, so no timer on output can tell them apart —
// it would fire in the middle of a quiet build. But input is answered: a live
// pty echoes a keystroke within milliseconds. Typing into a connection and
// getting nothing back is the thing that only happens when the socket is gone.
//
// Watching for that means sp has to sit in the byte stream, so it runs the
// client on a pty of its own and proxies the terminal through it.

const (
	// stallEchoGrace is how long an unanswered keystroke is tolerated before
	// the connection is called dead. Generous next to the milliseconds an echo
	// actually takes: the cost of being wrong is a needless reattach, but it
	// still has to be short enough to beat reaching for another terminal.
	stallEchoGrace = 12 * time.Second
	// stallPollEvery is how often the grace period is checked.
	stallPollEvery = time.Second
)

// stallDetector tracks whether input has gone unanswered long enough to call
// the connection dead.
//
// Only the oldest unanswered keystroke matters, so typing more while already
// waiting does not push the deadline back — otherwise holding a key down on a
// dead connection would keep it alive forever.
type stallDetector struct {
	grace time.Duration

	mu sync.Mutex
	// waitingSince is when the oldest unanswered input was sent. Zero means
	// everything typed so far has been answered.
	waitingSince time.Time
}

// sentInput records that keystrokes went to the sprite.
func (d *stallDetector) sentInput(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.waitingSince.IsZero() {
		d.waitingSince = now
	}
}

// sawOutput records that the sprite sent something back, which answers every
// keystroke outstanding at that point.
func (d *stallDetector) sawOutput() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.waitingSince = time.Time{}
}

// stalled reports whether input has gone unanswered past the grace period.
func (d *stallDetector) stalled(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.waitingSince.IsZero() {
		return false
	}
	return now.Sub(d.waitingSince) >= d.grace
}

// runProxied runs the sprite client on a pty that sp owns, copying between it
// and the real terminal, and kills it if the connection stops answering input.
//
// It reports whether the client was killed for stalling, which the caller
// treats as a dropped connection — because that is what it is, and the session
// on the sprite is untouched and waiting to be reattached.
//
// tap receives everything the client writes. With a pty there is no separate
// stderr to capture (both land in the one stream), so failure classification
// reads the combined output; the markers it looks for are distinctive enough
// not to collide with ordinary session text.
func runProxied(binary string, args []string, tap io.Writer) (bool, error) {
	cmd := exec.Command(binary, args...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return false, err
	}
	defer ptmx.Close()

	// The client renders for whatever size it is given, so the pty has to
	// track the real terminal rather than keep the 80x24 default it starts at.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	pty.InheritSize(os.Stdin, ptmx)

	// Raw mode moves to sp, since sp now owns the terminal the client used to
	// hold. Without it the local line discipline would eat keystrokes the
	// remote application expects to receive itself.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if prev, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			defer term.Restore(int(os.Stdin.Fd()), prev)
		}
	}

	det := &stallDetector{grace: stallEchoGrace}

	// Input is copied by hand rather than with io.Copy so each read can be
	// recorded. The goroutine is left running at exit: a read on the terminal
	// cannot be cancelled, and the process is either exiting or about to
	// reattach with a fresh pty.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				det.sentInput(time.Now())
				if _, err := ptmx.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				det.sawOutput()
				os.Stdout.Write(buf[:n])
				tap.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	var stalled atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(stallPollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				if det.stalled(now) {
					stalled.Store(true)
					cmd.Process.Kill()
					return
				}
			}
		}
	}()

	err = cmd.Wait()
	return stalled.Load(), err
}
