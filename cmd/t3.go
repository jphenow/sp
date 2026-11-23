package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/hub"
	"github.com/jphenow/sp/internal/relay"
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
	t3For       time.Duration
	t3Tailscale bool
	t3PairURL   string
	t3PairLabel string
	t3PairTTL   string
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
	Short: "Mint a code for connecting a T3 client to a sprite",
	Long: `pair mints a one-time code for adding a sprite as an environment in a T3
client. Run it once per client -- a desktop and a phone each need their own:

  sp t3 pair . --label desktop
  sp t3 pair . --label phone

The address it prints is the sprite's relay port when there is one: the host
in ~/.config/sp/relay.host and the sprite's line in ~/.config/sp/relay.list,
the same list sp relay serves. Going through the relay is what lets a client
wake a sleeping sprite. --url overrides the address.

With no relay configured it falls back to pairing over the sprite's own
tailnet address, which works only while the sprite is awake.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		spriteName, err := resolveSpriteTarget(args[0])
		if err != nil {
			return err
		}
		client := sprite.NewClient(globalOrg)
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite: spriteName, Org: globalOrg, Command: []string{"true"},
		}); err != nil {
			return fmt.Errorf("waking %s: %w", spriteName, err)
		}

		address := t3PairURL
		if address == "" {
			if address, err = relayAddressFor(spriteName); err != nil {
				return err
			}
		}
		if address == "" {
			fmt.Println("No relay route for this sprite; pairing over its tailnet address, which cannot wake it.")
			return pairT3(client, spriteName)
		}
		return pairT3At(client, spriteName, address)
	},
}

func init() {
	t3SetupCmd.Flags().DurationVar(&t3For, "for", time.Hour, "how long to hold the sprite Active once it has joined")
	t3SetupCmd.Flags().BoolVar(&t3Tailscale, "tailscale", false, "reach the sprite over your tailnet instead of T3 Connect")
	t3PairCmd.Flags().StringVar(&t3PairURL, "url", "", "address the client should use (default: the sprite's relay port)")
	t3PairCmd.Flags().StringVar(&t3PairLabel, "label", "sp", "name for this client in T3's connections list")
	t3PairCmd.Flags().StringVar(&t3PairTTL, "ttl", "15m", "how long the code stays valid")
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
	spriteName, err := resolveSpriteTarget(args[0])
	if err != nil {
		return err
	}
	client := sprite.NewClient(globalOrg)

	fmt.Printf("Waking %s...\n", spriteName)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg, Command: []string{"true"},
	}); err != nil {
		return fmt.Errorf("waking %s: %w", spriteName, err)
	}

	if err := ensureRestartableServices(client, spriteName, globalOrg); err != nil {
		return err
	}

	if err := ensureT3Installed(client, spriteName); err != nil {
		return err
	}

	// Stop whatever owns the data directory: the service if one exists, plus
	// any server started by hand over SSH, which the service does not know
	// about and which would otherwise hold the lock.
	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg,
		Command: []string{"sprite-env", "services", "delete", t3Service},
	})
	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg, Command: []string{"pkill", "-f", "t3 serve"},
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
		Sprite: spriteName, Org: globalOrg,
		Command: []string{"sh", "-lc", t3Bin + " connect publish"},
	})

	fmt.Println("Starting the relay service...")
	if err := hub.NewActions(globalOrg).StartT3(spriteName, t3For); err != nil {
		return err
	}

	out, _ := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg,
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
		Sprite: spriteName, Org: globalOrg,
		Command: []string{"sh", "-lc", "test -n \"" + t3Bin + "\""},
	}); err == nil {
		return nil
	}
	fmt.Println("Installing T3 on the sprite...")
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg,
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
		Sprite: spriteName, Org: globalOrg, TTY: true,
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
	if err := setup.JoinTailnet(client, spriteName, globalOrg, setup.TailnetOptions{
		AuthKey: key,
		Tags:    setup.TailscaleTags(),
	}); err != nil {
		return err
	}

	name, err := setup.TailnetName(client, spriteName, globalOrg)
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
		Sprite: spriteName, Org: globalOrg, Command: []string{"sh", "-c", create},
	}); err != nil {
		return fmt.Errorf("starting T3 on %s: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}

	// Two different holds, for two different problems. This one covers
	// pairing: without it the sprite pauses within seconds of this returning
	// and the tailnet address stops answering mid-pairing. It is a deadline
	// because pairing is a bounded, attended act.
	if err := hub.NewActions(globalOrg).SetHold(spriteName, t3For, true); err != nil {
		return err
	}

	// This one covers the unattended case the deadline cannot: an agent still
	// working after the hold lapses and the phone has disconnected. It holds
	// on evidence of work rather than on a clock, and releases when the work
	// stops, so the sprite sleeps on its own.
	fmt.Println("Installing the activity hold...")
	if err := setup.InstallAwakeService(client, spriteName, globalOrg, setup.T3BusyProbe); err != nil {
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
	//
	// The wait is on the runtime file rather than on a t3 subcommand because
	// the subcommands that looked like readiness probes are not: `t3 auth
	// pairing list` exits 0 with no server running at all, so the loop fell
	// straight through and `pair` failed with NoRunningServerError about a
	// second before the server finished starting. server-runtime.json is what
	// `pair` itself looks for -- the error names it -- so waiting on it asks
	// the same question pair is about to ask.
	script := fmt.Sprintf(`for i in $(seq 30); do
  [ -s "$HOME/.t3/userdata/server-runtime.json" ] && break
  sleep 1
done
exec %s pair --tailscale`, t3Bin)

	args := client.BuildExecArgs(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg, TTY: true,
		Command: []string{"sh", "-lc", script},
	})
	run := exec.Command(binary, args...)
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		return fmt.Errorf("pairing %s: %w", spriteName, err)
	}
	return nil
}

// pairT3At mints a pairing code whose link points at address, and prints the
// code, the address and the link.
//
// It uses T3's pairing-token command rather than t3 pair because pair only
// knows the server's own addresses; a relay is an address the server has
// never heard of.
func pairT3At(client *sprite.Client, spriteName, address string) error {
	// Same readiness wait as pairT3: the runtime file is what the pairing
	// commands look for.
	script := fmt.Sprintf(`for i in $(seq 30); do
  [ -s "$HOME/.t3/userdata/server-runtime.json" ] && break
  sleep 1
done
exec %s auth pairing create --ttl %s --label %s --base-url %s`,
		t3Bin, shellQuoteArg(t3PairTTL), shellQuoteArg(t3PairLabel), shellQuoteArg(address))
	out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg,
		Command: []string{"sh", "-lc", script},
	})
	if err != nil {
		return fmt.Errorf("minting a pairing code on %s: %w\n%s", spriteName, err, out)
	}
	fmt.Printf("\nAddress: %s\n%s", address, out)
	return nil
}

// relayAddressFor returns the relay URL for a sprite from the local relay
// configuration, or "" when there is none or it doesn't list the sprite.
//
// A list that exists but cannot be read is an error rather than "no relay":
// falling back would hand out an address that cannot wake the sprite, with
// nothing to say why.
func relayAddressFor(spriteName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	dir := filepath.Join(home, ".config", "sp")
	host, err := os.ReadFile(filepath.Join(dir, "relay.host"))
	if err != nil || strings.TrimSpace(string(host)) == "" {
		return "", nil
	}
	listPath := filepath.Join(dir, "relay.list")
	f, err := os.Open(listPath)
	if os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("reading %s: %w", listPath, err)
	}
	defer f.Close()
	routes, err := relay.ParseRoutes(f)
	if err != nil {
		return "", fmt.Errorf("%s: %w", listPath, err)
	}
	return relay.Address(strings.TrimSpace(string(host)), routes, spriteName), nil
}
