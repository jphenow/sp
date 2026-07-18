package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/sprite"
)

const (
	// shareServiceName is the sprite-env service that runs the web terminal
	// (ttyd or sshx, depending on whether the sprite's HTTP port is free).
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

// shareCmd exposes an already-running sprite tmux session as a web terminal you
// can join from a phone. When the sprite's single HTTP port is free it serves a
// ttyd terminal over the sprite's own Fly.io URL; when the sprite already runs
// an HTTP app on that port it falls back to sshx (an outbound relay that needs
// no port). Either way it attaches the SAME tmux session sp uses, so laptop and
// phone share one session.
var shareCmd = &cobra.Command{
	Use:   "share [target] [variant]",
	Short: "Share a running sprite session as a web terminal you can join from your phone",
	Long: `share runs a web terminal in the sprite as a sprite-env service and prints a
URL to open in a phone browser. It joins the SAME tmux session your laptop is
attached to.

Transport is chosen automatically:
  - HTTP port free: ttyd over the sprite's own Fly.io URL (stable, Fly routing).
  - HTTP port taken by your app: sshx relay (no port needed, external link).

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
// (--stop) or starts a web terminal — ttyd over the Fly URL if the HTTP port is
// free, otherwise sshx over its relay — attached to the running tmux session.
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

	stopHint := "sp share --stop"
	if len(args) > 0 {
		stopHint = "sp share " + strings.Join(args, " ") + " --stop"
	}

	// A sprite exposes exactly one HTTP port. If another service already owns it,
	// ttyd can't get a Fly URL — use the sshx relay, which needs no port.
	httpTaken, owner := spriteHasHTTPService(client, resolved.SpriteName)
	if httpTaken {
		fmt.Printf("Sprite's HTTP port is used by service %q; sharing over the sshx relay instead of the Fly URL.\n", owner)
		return shareViaSshx(client, resolved.SpriteName, session, stopHint)
	}
	return shareViaTtyd(client, resolved.SpriteName, session, stopHint)
}

// shareViaTtyd installs ttyd and registers it as the sp-term service on the
// sprite's HTTP port, reachable at the sprite's Fly URL.
func shareViaTtyd(client *sprite.Client, spriteName, session, stopHint string) error {
	if err := ensureTtyd(client, spriteName); err != nil {
		return err
	}
	deleteShareService(client, spriteName)

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
		Sprite:  spriteName,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating ttyd service: %w\n%s", err, string(out))
	}

	url := ""
	if info, err := client.Get(spriteName); err == nil && info != nil {
		url = info.URL
	}
	fmt.Printf("\nSharing tmux session %q on sprite %q (ttyd over the sprite's Fly URL).\n", session, spriteName)
	if url != "" {
		fmt.Printf("\n  \U0001F4F1 Open on your phone:  %s\n\n", url)
	} else {
		fmt.Println("\n  Service created, but could not read the sprite URL — run 'sprite url'.")
	}
	fmt.Printf("  Stop sharing: %s\n", stopHint)
	return nil
}

// sshx paths inside the sprite: a tmux-attach wrapper (sshx --shell execs a
// single program path), a run wrapper that launches sshx and redirects its URL
// to a file, and that URL file. sshx prints only the URL under --quiet and Rust
// line-buffers stdout, so the URL lands in the file immediately — the sprite-env
// service log stream doesn't reliably surface it, hence the file.
const (
	sshxURLFile     = "/home/sprite/.sp-share-url"
	sshxRunWrapper  = "/home/sprite/.sp-share-run.sh"
	sshxTmuxWrapper = "/home/sprite/.sp-share-tmux.sh"
)

// shareViaSshx installs sshx and registers it as the sp-term service (no HTTP
// port needed), writing its share URL to a file which we then read back.
func shareViaSshx(client *sprite.Client, spriteName, session, stopHint string) error {
	sshxPath, err := ensureSshx(client, spriteName)
	if err != nil {
		return err
	}

	// Write both wrappers and clear any stale URL. Session names are sanitized
	// (alnum/dash) upstream, so they're safe to interpolate.
	writeCmd := fmt.Sprintf(
		"printf '#!/bin/sh\\nexec tmux new-session -A -s %s\\n' > %s && chmod +x %s && "+
			"printf '#!/bin/sh\\nexec %s --quiet --name %s --shell %s > %s 2>&1\\n' > %s && chmod +x %s && "+
			"rm -f %s",
		session, sshxTmuxWrapper, sshxTmuxWrapper,
		sshxPath, session, sshxTmuxWrapper, sshxURLFile, sshxRunWrapper, sshxRunWrapper,
		sshxURLFile,
	)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", writeCmd},
	}); err != nil {
		return fmt.Errorf("writing sshx wrappers: %w", err)
	}

	deleteShareService(client, spriteName)

	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd %s --no-stream",
		shareServiceName, sshxRunWrapper,
	)
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating sshx service: %w\n%s", err, string(out))
	}

	// Poll the URL file until sshx has connected and written its link.
	readCmd := fmt.Sprintf(
		"for i in $(seq 1 30); do if grep -q 'https://' %s 2>/dev/null; then break; fi; sleep 0.5; done; cat %s 2>/dev/null",
		sshxURLFile, sshxURLFile,
	)
	out, _ := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", readCmd},
	})

	fmt.Printf("\nSharing tmux session %q on sprite %q (sshx relay).\n", session, spriteName)
	if url := extractURL(string(out)); url != "" {
		fmt.Printf("\n  \U0001F4F1 Open on your phone:  %s\n\n", url)
	} else {
		fmt.Printf("\n  Service started, but no URL appeared yet. Check with:\n    sprite exec -s %s -- cat %s\n", spriteName, sshxURLFile)
	}
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
// service name for messaging. On any parse error it reports false so we default
// to the ttyd/Fly path.
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

// extractURL returns the first whitespace-delimited http(s) token in s, or "".
func extractURL(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return strings.TrimRight(f, ".,)")
		}
	}
	return ""
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

// ensureSshx makes sure the sshx binary is present in the sprite and returns an
// absolute path to it, installing via the official one-liner on first use.
func ensureSshx(client *sprite.Client, spriteName string) (string, error) {
	if p := locateSshx(client, spriteName); p != "" {
		return p, nil
	}
	fmt.Println("Installing sshx in sprite (first use)...")
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", "curl -sSf https://sshx.io/get | sh"},
	}); err != nil {
		return "", fmt.Errorf("installing sshx: %w", err)
	}
	if p := locateSshx(client, spriteName); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("sshx installed but not found — install it into the sprite manually and retry")
}

// locateSshx probes PATH and common install dirs for sshx, returning an absolute
// path or "". sprite exec doesn't source login profiles, so we can't rely on
// PATH alone — check the well-known install locations too.
func locateSshx(client *sprite.Client, spriteName string) string {
	probe := `command -v sshx 2>/dev/null || for p in "$HOME/.local/bin/sshx" /usr/local/bin/sshx /usr/bin/sshx; do [ -x "$p" ] && echo "$p" && break; done`
	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", probe},
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
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
