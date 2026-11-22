package setup

import (
	"encoding/base64"
	"fmt"

	"github.com/jphenow/sp/internal/sprite"
)

const (
	// AwakeService is the sprite-env service that holds a sprite Active while
	// its harness is working.
	AwakeService = "sp-awake"

	// awakePoll is how often the probe runs, and awakeHold how long each
	// renewal buys. The hold is deliberately several poll intervals long so a
	// slow probe or a missed tick doesn't drop the sprite mid-run.
	awakePoll = 20
	awakeHold = "2m"

	// awakeGrace is how many consecutive idle polls must pass before the hold
	// is released. One quiet poll is not idleness -- it is the gap between an
	// agent finishing one tool call and starting the next.
	awakeGrace = 3

	// T3BusyProbe reports whether T3 has an agent running, by reading the
	// files T3 keeps for its provider processes rather than asking the server.
	//
	// A file read works while the server is saturated, needs no project (T3
	// threads can exist with none, and `thread list` returns nothing for
	// those), and survives the server restarting underneath it.
	T3BusyProbe = `grep -l '"processes":\[[^]]' ` +
		`"$HOME"/.t3/userdata/provider-processes/*.json >/dev/null 2>&1`
)

// InstallAwakeService installs the hold loop on a sprite.
//
// Why this exists: the guest documents that "a running sprite warm-stops after
// its idle timeout with no exec, shell, or open connection. Services alone do
// not keep it awake; keep-awake does." A harness working unattended therefore
// has roughly half a minute to live once the phone disconnects -- measured:
// a process writing every 5s died ~30s after the last exec, and did not
// survive the stop. Long-running work needs an explicit hold.
//
// The hold is renewed from inside the sprite rather than from a laptop,
// because the whole point is that nothing of yours is connected while the
// agent works. keep-awake caps at an hour per call and is renewable, so the
// loop re-arms a short hold for as long as the probe says there is work, and
// releases it once there isn't -- which is what lets the sprite sleep
// promptly instead of waiting out a timer someone guessed at.
//
// probe is any shell command; exit 0 means "working". It is a parameter
// rather than a constant so a different harness only needs a different
// probe, not a different service.
func InstallAwakeService(client *sprite.Client, spriteName, org, probe string) error {
	if probe == "" {
		return fmt.Errorf("no busy probe given for %s", AwakeService)
	}

	script := awakeScript(probe)

	// Recreate rather than reuse: the probe may have changed, and a service
	// that already exists makes create fail rather than update.
	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org,
		Command: []string{"sprite-env", "services", "delete", AwakeService},
	})

	create := fmt.Sprintf("sprite-env services create %s --restart always -- /bin/sh -lc %s",
		AwakeService, shellQuote(awakeCommand(script)))
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org, Command: []string{"sh", "-c", create},
	}); err != nil {
		return fmt.Errorf("installing %s on %s: %w\n%s", AwakeService, spriteName, err, string(out))
	}
	return nil
}

// awakeScript returns the hold loop for a probe.
//
// The probe is spliced in as the loop's condition rather than handed to a
// nested sh -c, so it passes through exactly one shell parse -- the one that
// runs the loop.
func awakeScript(probe string) string {
	// Only the transition into idleness releases the hold (idle = grace, not
	// >=), so a long quiet stretch costs one call rather than one per poll.
	return fmt.Sprintf(`idle=0
while true; do
  if { %s
  }; then
    idle=0
    sprite-env keep-awake --for %s >/dev/null 2>&1 || true
  else
    idle=$((idle+1))
    [ "$idle" = "%d" ] && { sprite-env keep-awake --release >/dev/null 2>&1 || true; }
  fi
  sleep %d
done`, probe, awakeHold, awakeGrace, awakePoll)
}

// awakeCommand wraps a script so that it reaches the service intact.
//
// The script is shipped base64-encoded because the route to a service's
// stored command does not preserve shell quoting: with the loop and its probe
// quoted in the ordinary way, the escaped single quotes came back altered,
// the probe's grep pattern lost its quotes, and the stored probe failed on
// every poll. The hold therefore never engaged during real work, and the
// sprite stopped half a minute after the phone disconnected, taking the
// agent's work with it. Base64 has no quotes, backslashes or spaces for any
// layer to reinterpret.
func awakeCommand(script string) string {
	return "echo " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | /bin/sh"
}
