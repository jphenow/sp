package cmd

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/sprite"
)

// keepAliveTaskName is the Tasks API task name sp uses to hold a sprite Active.
// A single fixed name means reconnects/restarts refresh the same hold rather
// than stacking duplicate tasks.
const keepAliveTaskName = "sp-keepalive"

// keepAliveGenFile holds the generation token of the heartbeat that currently
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
const keepAliveGenFile = "/tmp/sp-keepalive.gen"

var (
	keepAliveFor  time.Duration
	keepAliveStop bool
)

// keepaliveCmd holds a sprite in the Active state using the sprite Tasks API, so
// it doesn't idle-pause while you're working over the terminal or Remote
// Control. A held sprite never drops its connection or cold-restarts, which is
// what makes a session reliably reachable (including from a phone) without a
// manual wake.
var keepaliveCmd = &cobra.Command{
	Use:   "keepalive [target] [variant]",
	Short: "Hold a sprite Active (Tasks API) so it doesn't idle-pause while you work",
	Long: `keepalive registers a heartbeat against the sprite Tasks API so the sprite
stays in the Active state instead of pausing after ~30s idle. Use it when you
want a session to stay reachable without a manual wake — e.g. while driving it
from your phone over Remote Control.

  sp keepalive .                 # hold current dir's sprite (default 1h)
  sp keepalive owner/repo --for 2h
  sp keepalive owner/repo blue   # hold the 'blue' variant
  sp keepalive . --stop          # release the hold; sprite may pause again

The hold runs on the sprite and self-releases at the deadline, so a forgotten
keepalive stops billing on its own.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runKeepalive,
}

func init() {
	keepaliveCmd.Flags().DurationVar(&keepAliveFor, "for", time.Hour, "how long to hold the sprite Active (e.g. 30m, 2h)")
	keepaliveCmd.Flags().BoolVar(&keepAliveStop, "stop", false, "release the hold instead of starting it")
	rootCmd.AddCommand(keepaliveCmd)
}

// runKeepalive resolves the target sprite and either releases the hold (--stop)
// or launches a bounded heartbeat that keeps it Active for --for.
func runKeepalive(cmd *cobra.Command, args []string) error {
	resolved, err := resolveTarget(args)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}
	client := sprite.NewClient(resolved.Org)

	exists, err := client.Exists(resolved.SpriteName)
	if err != nil {
		return fmt.Errorf("checking sprite: %w", err)
	}
	if !exists {
		return fmt.Errorf("sprite %q does not exist — run 'sp %s' first", resolved.SpriteName, strings.Join(args, " "))
	}

	if keepAliveStop {
		stopKeepAlive(client, resolved.SpriteName, resolved.Org)
		fmt.Printf("Released keepalive hold on sprite %q.\n", resolved.SpriteName)
		return nil
	}

	// idleSession "" → no idle-exit: an explicit keepalive holds for the full
	// window regardless of terminal activity (that's the point of the command).
	if err := launchKeepAlive(resolved.SpriteName, resolved.Org, keepAliveFor, "", ""); err != nil {
		return fmt.Errorf("starting keepalive: %w", err)
	}
	fmt.Printf("Holding sprite %q Active for %s (self-releases after).\n", resolved.SpriteName, keepAliveFor)
	fmt.Printf("Release early: sp keepalive %s --stop\n", strings.Join(args, " "))
	return nil
}

// heartbeatScript builds the on-sprite shell loop that holds the sprite Active
// via the Tasks API. It refreshes the task every 15s (task expires in 2m, giving
// several missed-beat margin). It exits (releasing the hold) on the first of:
//   - deadlineEpoch reached (0 = no deadline / cap);
//   - idleSession non-empty AND its tmux pane unchanged for ~60s (keep-warm:
//     stop billing once Claude reaches an idle prompt);
//   - watchSession non-empty AND that tmux session no longer exists (session-
//     tied: hold while the session lives, release the moment it's killed).
// idleSession and watchSession are mutually exclusive in practice — idle-exit
// suits a foreground disconnect, session-tie suits "keep it reachable until I
// end the session" (e.g. --rc, where an idle prompt means you're on your phone).
func heartbeatScript(deadlineEpoch int64, idleSession, watchSession string) string {
	// One tick = 15s. Idle after 4 unchanged ticks (~60s). PUT refreshes an
	// existing task; on the first tick (or after expiry) PUT 404s and POST
	// creates it. Ownership is handed over via the generation file rather than
	// by signalling the previous loop — see keepAliveGenFile for why.
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
`, keepAliveGenFile, keepAliveTaskName, deadlineEpoch, idleSession, watchSession)
}

// launchKeepAlive starts the heartbeat as a detached sprite-exec process so it
// outlives the local sp process (same detach pattern as the keep-warm sentinel:
// Setpgid + released so a terminal SIGHUP doesn't reach it). dur of 0 means no
// deadline; idleSession enables idle-exit and watchSession enables session-tied
// release (see heartbeatScript).
func launchKeepAlive(spriteName, org string, dur time.Duration, idleSession, watchSession string) error {
	var deadline int64
	if dur > 0 {
		deadline = time.Now().Add(dur).Unix()
	}
	script := heartbeatScript(deadline, idleSession, watchSession)

	args := []string{"exec"}
	if org != "" {
		args = append(args, "-o", org)
	}
	args = append(args, "-s", spriteName, "--", "sh", "-c", script)

	c := exec.Command("sprite", args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
	if err := c.Start(); err != nil {
		return fmt.Errorf("starting keepalive sentinel: %w", err)
	}
	return c.Process.Release()
}

// stopKeepAlive releases the hold: clearing the generation file makes any
// running heartbeat exit on its next tick (within ~15s), and the task is deleted
// immediately so the sprite can idle-pause right away. Best-effort; errors are
// ignored. Note this deliberately does not kill anything — see keepAliveGenFile.
func stopKeepAlive(client *sprite.Client, spriteName, org string) {
	script := fmt.Sprintf(
		"rm -f '%[1]s'; sprite-env curl -X DELETE /v1/tasks/%[2]s >/dev/null 2>&1; true",
		keepAliveGenFile, keepAliveTaskName,
	)
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     org,
		Command: []string{"sh", "-c", script},
	})
}
