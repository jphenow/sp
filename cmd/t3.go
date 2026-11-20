package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/hub"
	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
)

const (
	// t3Bin resolves the T3 executable at run time. Its launcher installs a
	// self-contained binary under a version directory, so the path moves with
	// every upgrade and cannot be pinned.
	t3Bin = "$(ls -1d $HOME/.t3/runtime/versions/*/t3 2>/dev/null | tail -1)"
	// t3Service is the sprite-env service that keeps a sprite on the T3 relay.
	// It matches the name the hub's button manages so the two don't collide.
	t3Service = "sp-t3"
)

var (
	t3Org       string
	t3For       time.Duration
	t3Tailscale bool
)

// t3Cmd groups the T3 Code commands.
var t3Cmd = &cobra.Command{
	Use:   "t3",
	Short: "Join a sprite to T3 Code so it is reachable from the phone app",
}

// t3SetupCmd performs the one-time, interactive half of joining a sprite to T3.
var t3SetupCmd = &cobra.Command{
	Use:   "setup <sprite>",
	Short: "Authorize a sprite for T3 Connect and start its relay service",
	Long: `setup joins a sprite to T3 Connect, which is what makes it appear as an
environment in the T3 phone app.

It is interactive once per sprite: T3's device flow prints a URL and a code for
you to approve in a browser. Everything after that is a service, so a sprite
that goes cold rejoins on its own when it is woken — and the hub's "join t3"
button re-runs just the service half, with no browser needed.

  sp t3 setup .
  sp t3 setup jphenow/sp
  sp t3 setup gameservers --for 3h

T3 Connect reaches a sprite over a relay the sprite dials out to, so this needs
no port forwarding and nothing is exposed on the sprite's own URL. Note that
the relay serves an unauthenticated descriptor naming the environment and its
OS; the API behind it requires authorization.

  sp t3 setup gameservers --tailscale

--tailscale uses your own tailnet instead of T3 Connect: the sprite joins the
tailnet, T3 is served over Tailscale Serve on an HTTPS name only your devices
can resolve, and you pair by scanning a QR code. Nothing is registered with a
T3 account and no third-party relay is involved. It needs an auth key -- see
"sp tailscale setup". The one thing you give up is T3's push notifications,
which are a function of the relay.`,
	Args: cobra.ExactArgs(1),
	RunE: runT3Setup,
}

// t3PairCmd mints a fresh pairing code for a sprite already serving T3 over
// the tailnet. Pairing tokens are short-lived by design, so adding a second
// device later -- or retrying a scan that timed out -- needs a new one rather
// than a repeat of setup.
var t3PairCmd = &cobra.Command{
	Use:   "pair <sprite>",
	Short: "Print a new pairing QR code for a sprite serving T3 over the tailnet",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := sprite.NewClient(t3Org)
		spriteName, err := resolveSpriteTarget(client, args[0])
		if err != nil {
			return err
		}
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite: spriteName, Org: t3Org, Command: []string{"true"},
		}); err != nil {
			return fmt.Errorf("waking %s: %w", spriteName, err)
		}
		return pairT3(client, spriteName)
	},
}

func init() {
	t3SetupCmd.Flags().StringVar(&t3Org, "org", "", "Sprites organization (default: the CLI default)")
	t3PairCmd.Flags().StringVar(&t3Org, "org", "", "Sprites organization (default: the CLI default)")
	t3SetupCmd.Flags().DurationVar(&t3For, "for", time.Hour, "how long to hold the sprite Active once it has joined")
	t3SetupCmd.Flags().BoolVar(&t3Tailscale, "tailscale", false, "reach the sprite over your tailnet instead of T3 Connect")
	t3Cmd.AddCommand(t3SetupCmd)
	t3Cmd.AddCommand(t3PairCmd)
	rootCmd.AddCommand(t3Cmd)
}

// runT3Setup authorizes a sprite for T3 Connect and leaves it serving.
//
// The order matters and was arrived at the hard way:
//
//   - `t3 connect link` stages an authorization that is applied "on next
//     start", so the server has to be restarted afterwards or nothing changes.
//   - Only one server may own T3's data directory, so the existing one is
//     stopped before linking rather than after.
//   - The unlink is not redundant on a sprite that already looks linked. A
//     stale relay-side record reports "provisioned", answers over the relay,
//     and still never appears in the account's environment list; revoking it
//     and re-creating it is what makes the sprite show up. On a fresh sprite
//     the unlink is a no-op.
func runT3Setup(cmd *cobra.Command, args []string) error {
	client := sprite.NewClient(t3Org)
	spriteName, err := resolveSpriteTarget(client, args[0])
	if err != nil {
		return err
	}

	fmt.Printf("Waking %s...\n", spriteName)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org, Command: []string{"true"},
	}); err != nil {
		return fmt.Errorf("waking %s: %w", spriteName, err)
	}

	if err := ensureRestartableServices(client, spriteName, t3Org); err != nil {
		return err
	}

	if err := ensureT3Installed(client, spriteName); err != nil {
		return err
	}

	// Stop whatever owns the data directory: the service if one exists, plus
	// any server started by hand over SSH, which the service does not know
	// about and which would otherwise hold the lock.
	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org,
		Command: []string{"sprite-env", "services", "delete", t3Service},
	})
	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org, Command: []string{"pkill", "-f", "t3 serve"},
	})

	if t3Tailscale {
		return setupT3OverTailnet(client, spriteName)
	}

	if err := linkT3(client, spriteName); err != nil {
		return err
	}

	// Publishing is what sends agent activity to the phone as notifications
	// and Live Activities. It is cleared by the unlink above, so it is set
	// again here rather than left to be rediscovered as missing later.
	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org,
		Command: []string{"sh", "-lc", t3Bin + " connect publish"},
	})

	fmt.Println("Starting the relay service...")
	if err := hub.NewActions(t3Org).StartT3(spriteName, t3For); err != nil {
		return err
	}

	out, _ := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org,
		Command: []string{"sh", "-lc", t3Bin + " connect status"},
	})
	fmt.Printf("\n%s\n", out)
	fmt.Printf("%s should now be listed under T3 Connect in the app.\n", spriteName)
	return nil
}

// ensureRestartableServices checks that the sprite's guest runtime can create
// a service that restarts on its own.
//
// Older guests accept only --cmd/--args and have no --restart, and a service
// created there would not come back after a cold wake — which is the one
// property this whole arrangement depends on. Failing here beats creating a
// service that looks fine and quietly stops rejoining, and beats surfacing the
// guest's bare "unknown option: --restart".
func ensureRestartableServices(client *sprite.Client, spriteName, org string) error {
	out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org,
		Command: []string{"sprite-env", "services", "--help"},
	})
	if err == nil && strings.Contains(string(out), "--restart") {
		return nil
	}
	version, _ := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org, Command: []string{"sprite-env", "--version"},
	})
	return fmt.Errorf(
		"%s runs a guest runtime (%s) whose services cannot restart themselves.\n"+
			"A T3 service created there would not rejoin after the sprite woke cold.\n"+
			"Upgrade the sprite and run this again.",
		spriteName, lastLine(string(version)))
}

// lastLine returns the final non-empty line of command output. The sprite CLI
// writes advisories to stderr, which arrive interleaved with the output we
// actually want, and the value is always last.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return "unknown"
}

// ensureT3Installed installs the T3 launcher on the sprite if it is missing.
//
// The install is left to npx rather than a global npm install because T3's
// launcher manages its own versioned runtime under ~/.t3 either way, and npx
// needs no global write.
func ensureT3Installed(client *sprite.Client, spriteName string) error {
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org,
		Command: []string{"sh", "-lc", "test -n \"" + t3Bin + "\""},
	}); err == nil {
		return nil
	}
	fmt.Println("Installing T3 on the sprite...")
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org,
		Command: []string{"sh", "-lc", "npx -y t3 --version"},
	}); err != nil {
		return fmt.Errorf("installing T3 on %s: %w\n%s", spriteName, err, out)
	}
	return nil
}

// linkT3 runs T3's device-code authorization against the sprite with the
// terminal attached, so the code and its prompt reach the person running this.
//
// It runs as an interactive exec rather than through Client.Exec because the
// flow waits on approval in a browser and prints as it goes; a captured exec
// would show nothing until it had already timed out.
func linkT3(client *sprite.Client, spriteName string) error {
	binary, err := exec.LookPath("sprite")
	if err != nil {
		return fmt.Errorf("sprite binary not found: %w", err)
	}

	// A failed unlink is expected on a sprite that was never linked, so it is
	// chained with ';' rather than '&&'.
	script := t3Bin + " connect unlink; exec " + t3Bin + " connect link --headless"
	args := client.BuildExecArgs(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org, TTY: true,
		Command: []string{"sh", "-lc", script},
	})

	fmt.Printf("\nAuthorizing %s with T3 (approve the code it prints):\n\n", spriteName)
	run := exec.Command(binary, args...)
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("authorizing %s with T3: %w", spriteName, err)
	}
	return nil
}

// setupT3OverTailnet serves T3 on the sprite's tailnet address instead of
// registering it with T3 Connect.
//
// The relay path and this one are mutually exclusive by nature, not by policy:
// both want to be the single T3 server owning the data directory. The caller
// has already stopped whatever was running, so this starts the replacement.
//
// Pairing happens over the tailnet too. `t3 pair --tailscale` mints a
// short-lived token and prints it as a QR code, which is why it runs with a
// terminal attached -- a captured exec would hand back an unreadable blob of
// block characters instead of something a phone camera can read.
func setupT3OverTailnet(client *sprite.Client, spriteName string) error {
	key := setup.TailscaleAuthKey()
	if key == "" {
		return fmt.Errorf(
			"no tailnet auth key found.\n"+
				"Create a reusable, pre-approved, tagged key in the Tailscale admin console\n"+
				"and save it to:\n  %s",
			setup.TailscaleAuthKeyPath())
	}

	fmt.Printf("Joining %s to the tailnet...\n", spriteName)
	if err := setup.JoinTailnet(client, spriteName, t3Org, setup.TailnetOptions{
		AuthKey: key,
		Tags:    setup.TailscaleTags(),
	}); err != nil {
		return err
	}

	name, err := setup.TailnetName(client, spriteName, t3Org)
	if err != nil {
		return err
	}

	// HOME and PATH both matter: T3 keeps its runtime and credentials under
	// the home directory, and Tailscale Serve is configured by shelling out to
	// the tailscale binary, which lives in /usr/local/bin.
	env := "HOME=/home/sprite,PATH=/home/sprite/.local/bin:/usr/local/bin:/usr/bin:/bin"

	// `yes` answers the prompt T3 raises when its relay client is missing; a
	// service has no terminal, and unanswered it defaults to no and exits
	// having done nothing, which looks exactly like a service that failed.
	run := fmt.Sprintf("yes y | %s serve --no-browser --tailscale-serve", t3Bin)
	create := fmt.Sprintf("sprite-env services create %s --restart always --env %s -- /bin/sh -lc %s",
		t3Service, shellQuote(env), shellQuote(run))

	fmt.Println("Starting T3 on the tailnet...")
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org, Command: []string{"sh", "-c", create},
	}); err != nil {
		return fmt.Errorf("starting T3 on %s: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}

	// Without a hold the sprite pauses within seconds of this returning and
	// the tailnet address stops answering mid-pairing.
	if err := hub.NewActions(t3Org).SetHold(spriteName, t3For, true); err != nil {
		return err
	}

	fmt.Printf("\n%s is serving T3 at https://%s\n", spriteName, name)
	fmt.Println("Scan this from the T3 app (the code expires in 5 minutes):")
	fmt.Println()
	if err := pairT3(client, spriteName); err != nil {
		return fmt.Errorf("%w\n\nThe server is up -- you can retry pairing with:\n  sp t3 pair %s", err, spriteName)
	}
	return nil
}

// pairT3 prints a pairing QR code for a running T3 server on the sprite.
func pairT3(client *sprite.Client, spriteName string) error {
	binary, err := exec.LookPath("sprite")
	if err != nil {
		return fmt.Errorf("sprite binary not found: %w", err)
	}

	// The server has only just been registered as a service; wait for it to
	// answer before asking it for a token, or pairing fails on a race.
	script := fmt.Sprintf(`for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
  %s auth pairing list >/dev/null 2>&1 && break
  sleep 2
done
exec %s pair --tailscale`, t3Bin, t3Bin)

	args := client.BuildExecArgs(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org, TTY: true,
		Command: []string{"sh", "-lc", script},
	})
	run := exec.Command(binary, args...)
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("pairing %s: %w", spriteName, err)
	}
	return nil
}
