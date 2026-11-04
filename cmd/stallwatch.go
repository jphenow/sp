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
	// the connection is called dead. Enormous next to the milliseconds an echo
	// actually takes, and it has to be: a keystroke sent to a sprite that has
	// idle-paused is answered only once the sprite wakes, which was measured
	// at 11.7s. At the 12s this used to be, sp was deciding on a coin flip
	// whether a waking sprite was a dead connection — and killing it meant
	// another wake, which could be killed the same way.
	//
	// It is also deliberately past the point where ssh gives up on its own
	// (ServerAliveInterval 15 x 3), so on that transport the client's own
	// verdict comes first and this stays the backstop for the case ssh has no
	// answer for. The cost is that a genuinely dead connection now takes a
	// minute to notice, which is still the difference between waiting and
	// opening another terminal to kill sp by hand.
	stallEchoGrace = time.Minute
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

// stdinPump owns the terminal's input for as long as the reconnect loop runs,
// handing whatever it reads to the attempt that is currently live.
//
// Exactly one goroutine may read the terminal. A blocking read on a tty cannot
// be cancelled, so a reader started per attempt outlives its attempt and keeps
// consuming keystrokes afterwards — they are handed to a pty that is already
// closed and are simply gone. After a few reconnects most of what the user
// types disappears, the live connection sees no input, and the stall detector
// reads that silence as another dead connection: the loop then reconnects
// until the window expires even though the sprite is perfectly healthy.
type stdinPump struct {
	mu   sync.Mutex
	dest io.Writer
	det  *stallDetector
}

// newStdinPump starts reading the terminal. Input arriving while no attempt is
// attached is dropped, which is what happened before as well — between two
// attaches there is nothing to deliver it to.
func newStdinPump() *stdinPump {
	p := &stdinPump{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				p.deliver(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	return p
}

// attach points the pump at a new attempt's pty and stall detector.
func (p *stdinPump) attach(dest io.Writer, det *stallDetector) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dest, p.det = dest, det
}

// detach stops delivery, so a finished attempt's pty is never written to again.
func (p *stdinPump) detach() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dest, p.det = nil, nil
}

// deliver forwards input to the live attempt and records that it is awaiting an
// answer.
func (p *stdinPump) deliver(b []byte) {
	p.mu.Lock()
	dest, det := p.dest, p.det
	p.mu.Unlock()
	if dest == nil {
		return
	}
	det.sentInput(time.Now())
	dest.Write(b)
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
func runProxied(binary string, args []string, tap io.Writer, pump *stdinPump) (bool, error) {
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

	// Input arrives from the caller's pump rather than from a reader of this
	// attempt's own, so the keystrokes always land on the connection that is
	// actually live. See stdinPump.
	pump.attach(ptmx, det)
	defer pump.detach()

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
