package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/sprite"
)

const (
	// rcServiceName is the sprite-env service that runs Claude in Remote Control
	// server mode. As a service it auto-restarts on boot and after a cold wake,
	// so Remote Control re-registers itself without manual intervention.
	rcServiceName = "sp-rc"
	// claudeBin is the claude binary path on the sprite. sprite-env services
	// don't source login profiles, so the service --cmd needs an absolute path.
	claudeBin = "/home/sprite/.local/bin/claude"
)

var (
	rcFor  time.Duration
	rcStop bool
)

// rcCmd runs Claude in Remote Control server mode as a self-healing sprite-env
// service, and holds the sprite Active so the session stays reachable from your
// phone/browser without a manual wake. Unlike `sp connect --rc` (an interactive
// session you also share), this runs headless in the background and survives
// cold wakes — the service restarts and re-registers Remote Control on its own.
var rcCmd = &cobra.Command{
	Use:   "rc [target] [variant]",
	Short: "Run Claude Remote Control as a self-healing background service on the sprite",
	Long: `rc starts Claude in Remote Control server mode as a sprite-env service, so it
restarts (and re-registers Remote Control) after a cold wake, and holds the
sprite Active for a bounded window so it stays reachable from the Claude app or
claude.ai/code without a manual wake.

  sp rc .                 # current dir's sprite, held Active 1h
  sp rc owner/repo --for 3h
  sp rc owner/repo blue   # the 'blue' variant
  sp rc . --stop          # stop the service and release the hold

Requires a full claude.ai login on the sprite (Remote Control rejects the
inference-only setup-token). Experimental.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runRC,
}

func init() {
	rcCmd.Flags().DurationVar(&rcFor, "for", time.Hour, "how long to hold the sprite Active for Remote Control (e.g. 2h)")
	rcCmd.Flags().BoolVar(&rcStop, "stop", false, "stop the Remote Control service and release the hold")
	rootCmd.AddCommand(rcCmd)
}

// runRC resolves the target and either tears down the RC service + hold (--stop)
// or creates the service and starts a bounded keepalive.
func runRC(cmd *cobra.Command, args []string) error {
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

	if rcStop {
		client.Exec(sprite.ExecOptions{
			Sprite:  resolved.SpriteName,
			Command: []string{"sprite-env", "services", "delete", rcServiceName},
		})
		stopKeepAlive(client, resolved.SpriteName, resolved.Org)
		fmt.Printf("Stopped Remote Control service and released the hold on sprite %q.\n", resolved.SpriteName)
		return nil
	}

	// Recreate the service idempotently.
	client.Exec(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{"sprite-env", "services", "delete", rcServiceName},
	})

	// The service runs a wrapper (not claude directly) so it can resume the last
	// session with --continue but fall back to a fresh one when none exists yet
	// — otherwise the first start (and every start after a session is cleaned up)
	// crash-loops with "No recent session found". On later restarts (cold wake)
	// --continue succeeds and Remote Control re-registers the same session.
	wrapper := "/home/sprite/.sp-rc.sh"
	writeWrapper := fmt.Sprintf(
		"printf '#!/bin/sh\\ncd %[1]s 2>/dev/null || true\\n%[2]s remote-control --continue --name %[3]s || %[2]s remote-control --name %[3]s\\n' > %[4]s && chmod +x %[4]s",
		resolved.RemotePath, claudeBin, resolved.SpriteName, wrapper,
	)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{"sh", "-c", writeWrapper},
	}); err != nil {
		return fmt.Errorf("writing Remote Control wrapper: %w", err)
	}

	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd %s --dir %s --duration 3s",
		rcServiceName, wrapper, resolved.RemotePath,
	)
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating Remote Control service: %w\n%s", err, string(out))
	}

	// Hold the sprite Active so Remote Control stays connected and reachable.
	if err := launchKeepAlive(resolved.SpriteName, resolved.Org, rcFor, "", ""); err != nil {
		fmt.Printf("Warning: Remote Control service started but keepalive failed: %v\n", err)
	}

	fmt.Printf("\nRemote Control running on sprite %q (held Active for %s).\n", resolved.SpriteName, rcFor)
	fmt.Println("Open the Claude app → Code tab and find the session by the sprite name.")
	fmt.Printf("Stop: sp rc %s --stop\n", strings.Join(args, " "))
	return nil
}
