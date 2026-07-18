package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/sprite"
)

var shareSessionName string

// shareCmd exposes an already-running sprite tmux session as a public web
// terminal (via sshx) so it can be joined from a phone browser. It attaches the
// SAME tmux session `sp` uses, so laptop and phone drive one live session — no
// fork. sshx connects out to its relay and returns a shareable URL, so there are
// no inbound ports and no sprite HTTP-routing to configure.
var shareCmd = &cobra.Command{
	Use:   "share [target] [variant]",
	Short: "Share a running sprite session as a phone-joinable web terminal (sshx)",
	Long: `share serves an existing sprite's tmux session as a public web terminal
using sshx (outbound relay only — no inbound ports). Open the printed link in a
phone browser to join the SAME session your laptop is attached to.

Target and variant resolve exactly like 'sp connect', so share the same way you
connect:
  sp share .                     # current dir's sprite
  sp share owner/repo            # a repo sprite
  sp share owner/repo blue       # the 'blue' variant sprite

By default it attaches whichever tmux session is already running in the sprite.
Ctrl-C stops sharing; the tmux session keeps running.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runShare,
}

func init() {
	shareCmd.Flags().StringVar(&shareSessionName, "name", "", "tmux session name to attach (default: the running session)")
	rootCmd.AddCommand(shareCmd)
}

// runShare resolves the target sprite, finds the running tmux session, ensures
// sshx is installed, and runs sshx in the foreground attached to that session.
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

	// Pick the tmux session to attach: explicit --name, else the session already
	// running in the sprite (join existing), else the connect default ("bash").
	session := shareSessionName
	if session == "" {
		session = detectSpriteTmuxSession(client, resolved.SpriteName)
	}
	if session == "" {
		session = "bash"
	}

	sshxPath, err := ensureSshx(client, resolved.SpriteName)
	if err != nil {
		return err
	}

	// sshx's --shell execs a single program path, so write a tiny wrapper that
	// attaches (or creates) the tmux session and point sshx at it. Session names
	// are sanitized (alnum/dash) upstream, so they're safe to interpolate.
	wrapper := "/home/sprite/.sp-share-" + session + ".sh"
	writeWrapper := fmt.Sprintf(
		"printf '#!/bin/sh\\nexec tmux new-session -A -s %s\\n' > %s && chmod +x %s",
		session, shellQuote(wrapper), shellQuote(wrapper),
	)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{"sh", "-c", writeWrapper},
	}); err != nil {
		return fmt.Errorf("writing share wrapper: %w", err)
	}

	fmt.Printf("Sharing tmux session %q on sprite %q via sshx.\n", session, resolved.SpriteName)

	// Run sshx with --quiet so it emits just the share URL on stdout, then keeps
	// running. We capture that URL and surface it prominently, then keep the
	// process alive until the user interrupts. No TTY is allocated: sshx creates
	// its own pty for the tmux child, so a clean pipe makes the URL easy to parse.
	execArgs := client.BuildExecArgs(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{sshxPath, "--quiet", "--name", session, "--shell", wrapper},
	})
	binary, err := exec.LookPath("sprite")
	if err != nil {
		return fmt.Errorf("sprite binary not found: %w", err)
	}
	run := exec.Command(binary, execArgs...)
	run.Stderr = os.Stderr
	stdout, err := run.StdoutPipe()
	if err != nil {
		return fmt.Errorf("capturing sshx output: %w", err)
	}
	if err := run.Start(); err != nil {
		return fmt.Errorf("starting sshx: %w", err)
	}

	// Stream sshx output; the first URL line is the share link.
	go func() {
		scanner := bufio.NewScanner(stdout)
		printedURL := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if !printedURL && strings.HasPrefix(line, "http") {
				fmt.Printf("\n  \U0001F4F1 Open on your phone:  %s\n\n", line)
				fmt.Println("  Ctrl-C stops sharing (the tmux session keeps running).")
				printedURL = true
				continue
			}
			fmt.Println(line)
		}
	}()

	// Forward Ctrl-C to sshx so it tears down cleanly and Wait returns.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println("\nStopping share (session keeps running)...")
		if run.Process != nil {
			_ = run.Process.Signal(os.Interrupt)
		}
	}()

	runErr := run.Wait()
	resetTerminal()
	return runErr
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
