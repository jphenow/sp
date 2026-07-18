package cmd

import (
	"encoding/json"
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
// TLS, routing, auth, and auto-wake all come from Fly — no external relay. ttyd
// attaches the SAME tmux session sp uses, so laptop and phone share one session.
var shareCmd = &cobra.Command{
	Use:   "share [target] [variant]",
	Short: "Share a running sprite session as a web terminal over the sprite's Fly URL (ttyd)",
	Long: `share runs a ttyd web terminal inside the sprite as a sprite-env service and
routes the sprite's own Fly.io URL to it. Open that URL in a phone browser to
join the SAME tmux session your laptop is attached to.

Traffic goes through the sprite's Fly URL, so it's authenticated by Fly — no
external relay, no public unauthenticated link. A sprite has only one HTTP port,
so if another service (e.g. your own web app) already owns it, share can't run
until that port is free.

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
// (--stop) or installs ttyd, registers it on the sprite's HTTP port, and prints
// the sprite's Fly URL to open on a phone.
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

	// A sprite exposes exactly one HTTP port. If another service already owns it,
	// ttyd can't get a Fly URL — refuse rather than expose a public relay. Print
	// the reason directly: the root command silences returned errors.
	if taken, owner := spriteHasHTTPService(client, resolved.SpriteName); taken {
		fmt.Printf("Can't share sprite %q: its single HTTP port is already used by service %q,\n", resolved.SpriteName, owner)
		fmt.Println("so sp share can't route a terminal through the (Fly-authenticated) sprite URL.")
		fmt.Printf("Free the port first, e.g.:\n  sprite exec -s %s -- sprite-env services delete %s\n", resolved.SpriteName, owner)
		fmt.Println("or share a sprite that doesn't run its own web app.")
		return nil
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
	deleteShareService(client, resolved.SpriteName)

	// ttyd -W -p <port> tmux new-session -A -s <session>
	//   -W    writable terminal
	//   -p    listen on the routed port (ttyd binds all interfaces by default;
	//         -i takes an interface NAME like eth0, not an address, so it's omitted)
	//   tmux  attaches or creates the session — the "join".
	svcArgs := fmt.Sprintf("-W,-p,%d,tmux,new-session,-A,-s,%s", shareTtydPort, session)
	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd %s --args %s --http-port %d --duration 3s",
		shareServiceName, ttydBin, svcArgs, shareTtydPort,
	)
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating ttyd service: %w\n%s", err, string(out))
	}

	url := ""
	if info, err := client.Get(resolved.SpriteName); err == nil && info != nil {
		url = info.URL
	}

	stopHint := "sp share --stop"
	if len(args) > 0 {
		stopHint = "sp share " + strings.Join(args, " ") + " --stop"
	}

	fmt.Printf("\nSharing tmux session %q on sprite %q (ttyd over the sprite's Fly URL).\n", session, resolved.SpriteName)
	if url != "" {
		fmt.Printf("\n  \U0001F4F1 Open on your phone:  %s\n\n", url)
	} else {
		fmt.Println("\n  Service created, but could not read the sprite URL — run 'sprite url'.")
	}
	fmt.Println("  Authenticated by Fly; auto-wakes on access.")
	fmt.Printf("  Stop sharing: %s\n", stopHint)
	return nil
}

// stopShare deletes the sp-term sprite-env service, ending the share. The
// underlying tmux session is untouched.
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

// deleteShareService best-effort removes any existing sp-term service so a
// fresh create doesn't conflict. Errors are ignored (service may not exist).
func deleteShareService(client *sprite.Client, spriteName string) {
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sprite-env", "services", "delete", shareServiceName},
	})
}

// spriteHasHTTPService reports whether any service other than sp-term already
// owns an HTTP port on the sprite (only one service may). Returns the owning
// service name for messaging. On any parse error it reports false so we still
// attempt the ttyd path.
func spriteHasHTTPService(client *sprite.Client, spriteName string) (bool, string) {
	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sprite-env", "services", "list"},
	})
	if err != nil {
		return false, ""
	}
	var svcs []struct {
		Name     string `json:"name"`
		HTTPPort int    `json:"http_port"`
	}
	if err := json.Unmarshal(out, &svcs); err != nil {
		return false, ""
	}
	for _, s := range svcs {
		if s.Name == shareServiceName {
			continue
		}
		if s.HTTPPort != 0 {
			return true, s.Name
		}
	}
	return false, ""
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
