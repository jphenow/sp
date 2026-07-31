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

// keepAlivePidFile records the running heartbeat loop's PID on the sprite so a
// new heartbeat (or --stop) can replace it. We use a pid file rather than
// pkill-by-marker because the marker would live in the heartbeat script's own
// argv, so pkill would match and kill the loop the moment it started.
const keepAlivePidFile = "/tmp/sp-keepalive.pid"

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
	if err := launchKeepAlive(resolved.SpriteName, resolved.Org, keepAliveFor, ""); err != nil {
		return fmt.Errorf("starting keepalive: %w", err)
	}
	fmt.Printf("Holding sprite %q Active for %s (self-releases after).\n", resolved.SpriteName, keepAliveFor)
	fmt.Printf("Release early: sp keepalive %s --stop\n", strings.Join(args, " "))
	return nil
}

// heartbeatScript builds the on-sprite shell loop that holds the sprite Active
// via the Tasks API. It refreshes the task every 15s (task expires in 2m, giving
// several missed-beat margin). deadlineEpoch of 0 means run until idle/killed.
// When idleSession is non-empty, the loop watches that tmux session's pane and
// exits once it's been unchanged for ~60s, so keep-warm stops billing when
// Claude reaches an idle prompt.
func heartbeatScript(deadlineEpoch int64, idleSession string) string {
	// One tick = 15s. Idle after 4 unchanged ticks (~60s). PUT refreshes an
	// existing task; on the first tick (or after expiry) PUT 404s and POST
	// creates it. A pid file (not a pkill marker) replaces any prior loop, so
	// the loop can't kill itself by matching the marker in its own argv.
	return fmt.Sprintf(`PIDFILE='%[1]s'
if [ -f "$PIDFILE" ]; then kill "$(cat "$PIDFILE")" 2>/dev/null || true; fi
echo $$ > "$PIDFILE"
TASK='%[2]s'
DEADLINE=%[3]d
IDLE_SESSION='%[4]s'
LAST=''
IDLE=0
while :; do
  NOW=$(date +%%s)
  if [ "$DEADLINE" -ne 0 ] && [ "$NOW" -ge "$DEADLINE" ]; then break; fi
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
sprite-env curl -X DELETE "/v1/tasks/$TASK" >/dev/null 2>&1
rm -f "$PIDFILE"
`, keepAlivePidFile, keepAliveTaskName, deadlineEpoch, idleSession)
}

// launchKeepAlive starts the heartbeat as a detached sprite-exec process so it
// outlives the local sp process (same detach pattern as the keep-warm sentinel:
// Setpgid + released so a terminal SIGHUP doesn't reach it). dur of 0 means run
// until idle/killed; idleSession enables idle-exit (see heartbeatScript).
func launchKeepAlive(spriteName, org string, dur time.Duration, idleSession string) error {
	var deadline int64
	if dur > 0 {
		deadline = time.Now().Add(dur).Unix()
	}
	script := heartbeatScript(deadline, idleSession)

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

// stopKeepAlive kills any running heartbeat on the sprite and deletes the task,
// letting the sprite idle-pause again. Best-effort; errors are ignored.
func stopKeepAlive(client *sprite.Client, spriteName, org string) {
	script := fmt.Sprintf(
		"if [ -f '%[1]s' ]; then kill \"$(cat '%[1]s')\" 2>/dev/null; rm -f '%[1]s'; fi; "+
			"sprite-env curl -X DELETE /v1/tasks/%[2]s >/dev/null 2>&1; true",
		keepAlivePidFile, keepAliveTaskName,
	)
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     org,
		Command: []string{"sh", "-c", script},
	})
}
