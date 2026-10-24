package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/hub"
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
	t3Org string
	t3For time.Duration
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

  sp t3 setup gameservers
  sp t3 setup gameservers --for 3h

T3 Connect reaches a sprite over a relay the sprite dials out to, so this needs
no port forwarding and nothing is exposed on the sprite's own URL. Note that
the relay serves an unauthenticated descriptor naming the environment and its
OS; the API behind it requires authorization.`,
	Args: cobra.ExactArgs(1),
	RunE: runT3Setup,
}

func init() {
	t3SetupCmd.Flags().StringVar(&t3Org, "org", "", "Sprites organization (default: the CLI default)")
	t3SetupCmd.Flags().DurationVar(&t3For, "for", time.Hour, "how long to hold the sprite Active once it has joined")
	t3Cmd.AddCommand(t3SetupCmd)
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
	spriteName := args[0]
	client := sprite.NewClient(t3Org)

	fmt.Printf("Waking %s...\n", spriteName)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: t3Org, Command: []string{"true"},
	}); err != nil {
		return fmt.Errorf("waking %s: %w", spriteName, err)
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
