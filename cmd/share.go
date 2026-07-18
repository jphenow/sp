package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/sprite"
)

const (
	// shareServiceName is the sprite-env service that runs the web terminal.
	shareServiceName = "sp-term"
	// shareTtydPort is the port ttyd listens on inside the sprite; the sprite
	// proxy routes the sprite's public URL to it via --http-port.
	shareTtydPort = 7681
	// ttydBin is where we install ttyd. sprite-env services don't source
	// .bashrc, so the service --cmd needs an absolute path.
	ttydBin = "/home/sprite/.local/bin/ttyd"
	// ttydRelease is the static (musl) linux/amd64 ttyd binary. Sprites are
	// linux/amd64 (see Makefile build-sprite).
	ttydRelease = "https://github.com/tsl0922/ttyd/releases/latest/download/ttyd.x86_64"
)

var (
	shareSessionName string
	shareStop        bool
)

// shareCmd exposes an already-running sprite tmux session as a web terminal
// reachable over the sprite's own Fly.io URL. It registers a ttyd process as a
// sprite-env service (like the opencode --web flow), so the HTTP networking,
// TLS, routing, and auto-wake all come from Fly — no third-party relay. ttyd
// attaches the SAME tmux session sp uses, so laptop and phone share one session.
var shareCmd = &cobra.Command{
	Use:   "share [target] [variant]",
	Short: "Share a running sprite session as a web terminal over the sprite's Fly URL (ttyd)",
	Long: `share runs a ttyd web terminal inside the sprite as a sprite-env service and
routes the sprite's own Fly.io URL to it. Open that URL in a phone browser to
join the SAME tmux session your laptop is attached to.

Because it's a sprite service, the URL is stable and auto-wakes the sprite on
access — the HTTP networking comes from Fly, not an external relay.

Target and variant resolve exactly like 'sp connect':
  sp share .                     # current dir's sprite
  sp share owner/repo            # a repo sprite
  sp share owner/repo blue       # the 'blue' variant sprite

By default it attaches whichever tmux session is already running in the sprite.
Stop sharing with the same target plus --stop.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runShare,
}

func init() {
	shareCmd.Flags().StringVar(&shareSessionName, "name", "", "tmux session name to attach (default: the running session)")
	shareCmd.Flags().BoolVar(&shareStop, "stop", false, "tear down the share service instead of starting it")
	rootCmd.AddCommand(shareCmd)
}

// runShare resolves the target sprite, then either tears down the share service
// (--stop) or installs ttyd, registers the service against the running tmux
// session, and prints the sprite's Fly URL to open on a phone.
func runShare(cmd *cobra.Command, args []string) error {
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

	if shareStop {
		return stopShare(client, resolved.SpriteName)
	}

	// Pick the tmux session to attach: explicit --name, else the session already
	// running in the sprite (join existing), else the connect default ("bash").
	session := shareSessionName
	if session == "" {
		session = detectSpriteTmuxSession(client, resolved.SpriteName)
	}
	if session == "" {
		session = "bash"
	}

	if err := ensureTtyd(client, resolved.SpriteName); err != nil {
		return err
	}
	if err := createShareService(client, resolved.SpriteName, session); err != nil {
		return err
	}

	url := ""
	if info, err := client.Get(resolved.SpriteName); err == nil && info != nil {
		url = info.URL
	}

	stopHint := "sp share --stop"
	if len(args) > 0 {
		stopHint = "sp share " + strings.Join(args, " ") + " --stop"
	}

	fmt.Printf("\nSharing tmux session %q on sprite %q.\n", session, resolved.SpriteName)
	if url != "" {
		fmt.Printf("\n  \U0001F4F1 Open on your phone:  %s\n\n", url)
	} else {
		fmt.Println("\n  Service created, but could not read the sprite URL — run 'sp status' or 'sprite url'.")
	}
	fmt.Println("  Served over the sprite's Fly URL; auto-wakes on access.")
	fmt.Printf("  Stop sharing: %s\n", stopHint)
	return nil
}

// stopShare deletes the sp-term sprite-env service, ending the web-terminal
// share. The underlying tmux session is untouched.
func stopShare(client *sprite.Client, spriteName string) error {
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sprite-env", "services", "delete", shareServiceName},
	}); err != nil {
		return fmt.Errorf("stopping share service: %w", err)
	}
	fmt.Printf("Stopped sharing sprite %q (tmux session still running).\n", spriteName)
	return nil
}

// createShareService registers a ttyd sprite-env service that serves the given
// tmux session on the sprite's HTTP port. Reconfigures idempotently by deleting
// any prior sp-term service first. Session names are sanitized (alnum/dash)
// upstream, so they're safe to place in the comma-separated args list.
func createShareService(client *sprite.Client, spriteName, session string) error {
	// Best-effort delete of any existing service so create doesn't conflict.
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sprite-env", "services", "delete", shareServiceName},
	})

	// ttyd -W -p <port> -i 0.0.0.0 tmux new-session -A -s <session>
	//   -W            allow client input (writable terminal)
	//   -p/-i         listen on the routed port, all interfaces (sprite proxy
	//                 connects from outside localhost)
	//   tmux ...      attaches or creates the session — this is the "join".
	svcArgs := fmt.Sprintf("-W,-p,%d,-i,0.0.0.0,tmux,new-session,-A,-s,%s", shareTtydPort, session)
	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd %s --args %s --http-port %d --duration 10s",
		shareServiceName, ttydBin, svcArgs, shareTtydPort,
	)

	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", createCmd},
	})
	if err != nil {
		return fmt.Errorf("creating share service: %w\n%s", err, string(out))
	}
	return nil
}

// ensureTtyd makes sure the ttyd binary exists at ttydBin inside the sprite,
// preferring a system copy and falling back to the static release download.
func ensureTtyd(client *sprite.Client, spriteName string) error {
	script := fmt.Sprintf(`set -e
if [ -x %[1]s ]; then exit 0; fi
mkdir -p "$(dirname %[1]s)"
if command -v ttyd >/dev/null 2>&1; then
  ln -sf "$(command -v ttyd)" %[1]s
  exit 0
fi
curl -fsSL -o %[1]s %[2]s
chmod +x %[1]s
`, ttydBin, ttydRelease)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", script},
	}); err != nil {
		return fmt.Errorf("installing ttyd in sprite: %w", err)
	}
	return nil
}

// detectSpriteTmuxSession returns the name of the first running tmux session in
// the sprite, or "" if tmux isn't running / has no sessions. This is what makes
// `sp share` join an existing sp session by default.
func detectSpriteTmuxSession(client *sprite.Client, spriteName string) string {
	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"tmux", "list-sessions", "-F", "#{session_name}"},
	})
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
