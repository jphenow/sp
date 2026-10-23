// Package keepalive holds a sprite in the Active state via the sprite Tasks
// API, so it doesn't idle-pause while someone is working on it.
//
// It lives in its own package because two callers need it: the CLI commands
// that hold a sprite for an interactive session, and the hub, which holds a
// sprite on behalf of a phone that cannot keep a connection open.
package keepalive

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/jphenow/sp/internal/sprite"
)

// TaskName is the Tasks API task name sp uses to hold a sprite Active.
// A single fixed name means reconnects/restarts refresh the same hold rather
// than stacking duplicate tasks.
const TaskName = "sp-keepalive"

// GenFile holds the generation token of the heartbeat that currently
// owns the hold. A new heartbeat claims ownership by overwriting it; each loop
// re-reads the file every tick and exits when it no longer holds its own token.
//
// This is deliberately a cooperative handshake rather than killing the previous
// loop by PID: sprite PIDs are small and reset on every cold restart, while
// files under /tmp survive, so a stale PID file routinely points at an unrelated
// process by the time we'd act on it — including the user's tmux server (tmux
// runs at PIDs like 114/396 on a sprite). Killing it took down tmux and Claude,
// which then looked like "my session was replaced by a fresh shell". Never kill
// a PID read from a file that outlives the process namespace.
const GenFile = "/tmp/sp-keepalive.gen"

// Options configures a hold.
type Options struct {
	// Sprite is the sprite to hold Active.
	Sprite string
	// Org is the sprite's organization; empty uses the CLI default.
	Org string
	// For is how long to hold. Zero means no deadline.
	For time.Duration
	// IdleSession enables idle-exit: the hold releases once that tmux pane
	// has been unchanged for ~60s.
	IdleSession string
	// WatchSession ties the hold to a tmux session's lifetime: it releases
	// the moment that session disappears.
	WatchSession string
}

// Script builds the on-sprite shell loop that holds the sprite Active
// via the Tasks API. It refreshes the task every 15s (task expires in 2m, giving
// several missed-beat margin). It exits (releasing the hold) on the first of:
//   - deadlineEpoch reached (0 = no deadline / cap);
//   - idleSession non-empty AND its tmux pane unchanged for ~60s (keep-warm:
//     stop billing once Claude reaches an idle prompt);
//   - watchSession non-empty AND that tmux session no longer exists (session-
//     tied: hold while the session lives, release the moment it's killed).
//
// idleSession and watchSession are mutually exclusive in practice — idle-exit
// suits a foreground disconnect, session-tie suits "keep it reachable until I
// end the session" (e.g. --rc, where an idle prompt means you're on your phone).
func Script(deadlineEpoch int64, idleSession, watchSession string) string {
	// One tick = 15s. Idle after 4 unchanged ticks (~60s). PUT refreshes an
	// existing task; on the first tick (or after expiry) PUT 404s and POST
	// creates it. Ownership is handed over via the generation file rather than
	// by signalling the previous loop — see GenFile for why.
	return fmt.Sprintf(`GENFILE='%[1]s'
GEN="$$-$(date +%%s)"
echo "$GEN" > "$GENFILE"
TASK='%[2]s'
DEADLINE=%[3]d
IDLE_SESSION='%[4]s'
WATCH_SESSION='%[5]s'
LAST=''
IDLE=0
SEEN=0
GONE=0
while :; do
  # Step aside if a newer heartbeat claimed the hold, or --stop cleared it.
  [ "$(cat "$GENFILE" 2>/dev/null)" = "$GEN" ] || exit 0
  NOW=$(date +%%s)
  if [ "$DEADLINE" -ne 0 ] && [ "$NOW" -ge "$DEADLINE" ]; then break; fi
  # Session-tie: hold until the watched session exists and then disappears.
  # SEEN avoids exiting during the startup window before the session is created;
  # GONE requires several consecutive misses so a transient tmux hiccup (e.g.
  # while the sprite resumes from a warm pause) doesn't drop the hold for good.
  if [ -n "$WATCH_SESSION" ]; then
    if tmux has-session -t "$WATCH_SESSION" 2>/dev/null; then
      SEEN=1
      GONE=0
    elif [ "$SEEN" -eq 1 ]; then
      GONE=$((GONE + 1))
      if [ "$GONE" -ge 4 ]; then break; fi
    fi
  fi
  if [ -n "$IDLE_SESSION" ]; then
    H=$(tmux capture-pane -t "$IDLE_SESSION" -p 2>/dev/null | sha256sum | cut -d' ' -f1)
    if [ "$H" = "$LAST" ]; then
      IDLE=$((IDLE + 1))
      if [ "$IDLE" -ge 4 ]; then break; fi
    else
      IDLE=0
    fi
    LAST="$H"
  fi
  sprite-env curl -X PUT "/v1/tasks/$TASK" -d '{"expire":"2m"}' >/dev/null 2>&1 \
    || sprite-env curl -X POST /v1/tasks -d "{\"name\":\"$TASK\",\"expire\":\"2m\"}" >/dev/null 2>&1
  sleep 15
done
# Only release the task if we still own the hold — a newer heartbeat may have
# taken over while we were finishing up.
if [ "$(cat "$GENFILE" 2>/dev/null)" = "$GEN" ]; then
  sprite-env curl -X DELETE "/v1/tasks/$TASK" >/dev/null 2>&1
  rm -f "$GENFILE"
fi
`, GenFile, TaskName, deadlineEpoch, idleSession, watchSession)
}

// Start launches the heartbeat as a detached sprite-exec process so it
// outlives the local sp process (same detach pattern as the keep-warm sentinel:
// Setpgid + released so a terminal SIGHUP doesn't reach it).
func Start(opts Options) error {
	var deadline int64
	if opts.For > 0 {
		deadline = time.Now().Add(opts.For).Unix()
	}
	script := Script(deadline, opts.IdleSession, opts.WatchSession)

	args := []string{"exec"}
	if opts.Org != "" {
		args = append(args, "-o", opts.Org)
	}
	args = append(args, "-s", opts.Sprite, "--", "sh", "-c", script)

	c := exec.Command("sprite", args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
	if err := c.Start(); err != nil {
		return fmt.Errorf("starting keepalive sentinel: %w", err)
	}
	return c.Process.Release()
}

// Stop releases the hold: clearing the generation file makes any
// running heartbeat exit on its next tick (within ~15s), and the task is deleted
// immediately so the sprite can idle-pause right away. Best-effort; errors are
// ignored. Note this deliberately does not kill anything — see GenFile.
func Stop(client *sprite.Client, spriteName, org string) {
	script := fmt.Sprintf(
		"rm -f '%[1]s'; sprite-env curl -X DELETE /v1/tasks/%[2]s >/dev/null 2>&1; true",
		GenFile, TaskName,
	)
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     org,
		Command: []string{"sh", "-c", script},
	})
}
