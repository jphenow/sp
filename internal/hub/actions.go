package hub

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jphenow/sp/internal/keepalive"
	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
)

const (
	// hubTmuxSession is the tmux session the hub starts Claude in. A fixed
	// name keeps repeat taps idempotent and keeps hub-started work visibly
	// separate from a session you attached to yourself from a terminal.
	hubTmuxSession = "sp-hub"
	// claudeBin is the Claude binary path on a sprite. Services and non-login
	// shells don't source a profile, so this has to be absolute.
	claudeBin = "/home/sprite/.local/bin/claude"
	// defaultWorkDir is where Claude starts when nothing better is known.
	defaultWorkDir = "/home/sprite"
	// rcServiceName is the sprite-env service that runs Claude in Remote
	// Control mode, matching the name `sp rc` uses so the two don't collide.
	rcServiceName = "sp-rc"
	// spRemotePath is where the sp binary is installed on a sprite.
	spRemotePath = "/usr/local/bin/sp"
	// t3ServiceName is the sprite-env service that keeps the sprite joined to
	// the T3 relay.
	t3ServiceName = "sp-t3"
	// t3BinGlob finds the T3 executable. Its launcher installs a self-
	// contained binary under a version directory, so the path moves with each
	// upgrade and has to be resolved at run time rather than pinned.
	t3BinGlob = "$(ls -1d $HOME/.t3/runtime/versions/*/t3 2>/dev/null | tail -1)"
	// agentHome is the home directory the agent reads Claude's transcripts
	// from. Services start with no HOME, so it is passed in.
	agentHome = "/home/sprite"
	// agentPath is the PATH the agent runs with. It needs tmux, and services
	// start with a PATH that may not include the sprite's own bin directories.
	agentPath = "/home/sprite/.local/bin:/usr/local/bin:/usr/bin:/bin"
)

// Actions performs ladder operations on sprites, on behalf of the hub page.
type Actions struct {
	client *sprite.Client
	org    string
}

// NewActions builds an action runner against one organization.
func NewActions(org string) *Actions {
	return &Actions{client: sprite.NewClient(org), org: org}
}

// Wake brings a sprite's machine up. Any exec does it — the Sprites platform
// starts a cold machine to serve the call — so this runs the cheapest possible
// command and lets the platform do the work.
func (a *Actions) Wake(spriteName string) error {
	if _, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"true"},
	}); err != nil {
		return fmt.Errorf("waking %s: %w", spriteName, err)
	}
	return nil
}

// StartClaudeOptions configures starting a Claude session on a sprite.
type StartClaudeOptions struct {
	// Sprite is the sprite to start Claude on.
	Sprite string
	// Dir is the working directory; empty uses defaultWorkDir.
	Dir string
	// SessionID resumes a specific session. Empty means --continue, which
	// picks up the most recent session in Dir.
	SessionID string
}

// StartClaude starts Claude in a tmux session on the sprite.
//
// tmux is what makes this survive the phone: the browser tab can close, the
// network can drop, and Claude keeps running inside the session. The command
// falls back to a bare `claude` because --continue and --resume both fail hard
// when there is no matching session, which would otherwise leave an empty pane
// and no obvious reason why.
func (a *Actions) StartClaude(opts StartClaudeOptions) error {
	dir := opts.Dir
	if dir == "" {
		dir = defaultWorkDir
	}
	if err := validatePath(dir); err != nil {
		return err
	}

	resume := "--continue"
	if opts.SessionID != "" {
		if err := validateSessionID(opts.SessionID); err != nil {
			return err
		}
		resume = "--resume " + opts.SessionID
	}

	// An existing session gets a new window rather than being disturbed: the
	// user may be mid-task in it, and clobbering that is the one thing a
	// "revive my session" button must never do.
	script := fmt.Sprintf(`set -e
SESSION=%[1]s
DIR=%[2]s
CMD='%[3]s %[4]s || %[3]s'
if tmux has-session -t "$SESSION" 2>/dev/null; then
  tmux new-window -t "$SESSION" -c "$DIR" "$CMD"
else
  tmux new-session -d -s "$SESSION" -c "$DIR" "$CMD"
fi
`, hubTmuxSession, shellQuote(dir), claudeBin, resume)

	out, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  opts.Sprite,
		Org:     a.org,
		Command: []string{"sh", "-c", script},
	})
	if err != nil {
		return fmt.Errorf("starting Claude on %s: %w\n%s", opts.Sprite, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetHold starts or releases a Tasks API hold on the sprite.
//
// The hub owns the hold rather than the page, because a phone browser tab is
// not a reliable heartbeat: iOS suspends background tabs, which would drop the
// hold exactly when the phone is put down mid-task.
func (a *Actions) SetHold(spriteName string, dur time.Duration, on bool) error {
	if !on {
		keepalive.Stop(a.client, spriteName, a.org)
		return nil
	}
	if err := keepalive.Start(keepalive.Options{
		Sprite: spriteName,
		Org:    a.org,
		For:    dur,
	}); err != nil {
		return fmt.Errorf("holding %s: %w", spriteName, err)
	}
	return nil
}

// StartRemote registers the Remote Control service on a sprite, the same way
// `sp rc` does: as a sprite-env service, so it restarts and re-registers after
// a cold wake instead of needing another visit to this page.
func (a *Actions) StartRemote(spriteName, dir string) error {
	if dir == "" {
		dir = defaultWorkDir
	}
	if err := validatePath(dir); err != nil {
		return err
	}

	// Recreate idempotently: delete first so a repeat tap refreshes the
	// service rather than failing on a name clash.
	a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"sprite-env", "services", "delete", rcServiceName},
	})

	wrapper := "/home/sprite/.sp-rc.sh"
	writeWrapper := fmt.Sprintf(
		"printf '#!/bin/sh\\ncd %[1]s 2>/dev/null || true\\n%[2]s remote-control --continue --name %[3]s || %[2]s remote-control --name %[3]s\\n' > %[4]s && chmod +x %[4]s",
		dir, claudeBin, spriteName, wrapper,
	)
	if _, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"sh", "-c", writeWrapper},
	}); err != nil {
		return fmt.Errorf("writing Remote Control wrapper on %s: %w", spriteName, err)
	}

	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd %s --dir %s --duration 3s",
		rcServiceName, wrapper, shellQuote(dir),
	)
	if out, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating Remote Control service on %s: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StartT3 brings the sprite back onto the T3 relay and holds it Active.
//
// Two T3 commands are involved and only one of them runs here. `t3 connect`
// authorizes the machine against a T3 account; it is interactive, it is done
// once, and it exits. `t3 serve` is the process that actually makes the
// machine reachable, and it is what this (re)starts. A server started before
// the machine was authorized does not pick the authorization up, so this
// stops any server already running rather than leaving it in place.
//
// T3's mobile client reaches a machine over a relay the machine dials out to,
// so a sprite that has paused simply disappears from the phone with no way to
// call it back — the phone cannot wake a sprite and T3 has no hook to try.
// This is that missing call: it wakes the sprite, re-registers the relay
// service, and takes a hold.
//
// The hold is part of the action rather than a separate tap because without
// it the sprite pauses again within seconds of this returning and the client
// drops right back off, which makes the button look broken.
//
// Signing in to T3 is not done here. That is a one-time interactive flow whose
// credentials then live on the sprite; this only restarts a sprite that has
// already been signed in.
func (a *Actions) StartT3(spriteName string, hold time.Duration) error {
	if err := a.Wake(spriteName); err != nil {
		return err
	}

	// Recreate idempotently, matching the other service actions: a repeat tap
	// should refresh the service rather than fail on the name.
	a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"sprite-env", "services", "delete", t3ServiceName},
	})

	// HOME matters more than usual here: T3 keeps its runtime, its relay
	// client and its credentials under the home directory, and a service that
	// starts without one would come up unable to find any of them.
	env := fmt.Sprintf("HOME=%s,PATH=%s", agentHome, agentPath)

	// Only one server may own T3's data directory, so an existing one is
	// stopped first. T3's own `service` subcommands are systemd-user based and
	// systemd user sessions aren't available here, which is the whole reason
	// this runs as a sprite service instead.
	a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"pkill", "-f", "t3 serve"},
	})

	// `yes` answers the prompt T3 raises when its relay client is missing
	// ("Download and install version ...? (y/N)"). A service has no terminal,
	// so unanswered it defaults to no and the command exits having done
	// nothing — which looks exactly like a service that started and failed.
	//
	// The binary is resolved by glob rather than run through npx: npx would
	// re-resolve and possibly re-download the launcher on every restart, and
	// a cold-woken sprite restarting its services is the common case here.
	run := fmt.Sprintf("yes y | %s serve --no-browser", t3BinGlob)
	createCmd := fmt.Sprintf(
		"sprite-env services create %s --restart always --env %s -- /bin/sh -lc %s",
		t3ServiceName, shellQuote(env), shellQuote(run),
	)
	if out, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("starting the T3 service on %s: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}

	return a.SetHold(spriteName, hold, true)
}

// PushCredentials re-installs Claude credentials on a sprite from the hub's
// vault copy. This is the answer to "Claude is dead because its login expired"
// when there is no laptop around to run /login — the browser callback in that
// flow lands on localhost, which a phone cannot reach.
func (a *Actions) PushCredentials(spriteName string, creds []byte) error {
	if len(creds) == 0 {
		return fmt.Errorf("no credentials in the vault — run 'sp hub vault --save' from a machine that is logged in")
	}
	ok, msg := setup.SyncClaudeCredentials(a.client, spriteName, creds)
	if !ok {
		return fmt.Errorf("pushing credentials to %s: %s", spriteName, msg)
	}
	return nil
}

// InstallAgent uploads the sp binary and registers the reporting agent as a
// sprite-env service, so it comes back by itself after a cold wake.
func (a *Actions) InstallAgent(opts InstallAgentOptions) error {
	if err := validateURL(opts.HubURL); err != nil {
		return err
	}
	if err := a.client.UploadBinary(opts.Sprite, a.org, opts.BinaryPath, spRemotePath); err != nil {
		return err
	}

	a.client.Exec(sprite.ExecOptions{
		Sprite:  opts.Sprite,
		Org:     a.org,
		Command: []string{"sprite-env", "services", "delete", AgentServiceName},
	})

	// The token goes in the service environment rather than on the command
	// line, where it would show up in every process listing on the sprite.
	//
	// HOME and PATH have to be set explicitly too: services don't source a
	// login profile, and without them the agent can't find the home directory
	// holding Claude's transcripts (so it reports no resumable sessions) or
	// the tmux binary (so it reports tmux as down on a sprite where it is up).
	env := fmt.Sprintf("SP_HUB_TOKEN=%s,HOME=%s,PATH=%s",
		opts.Token, agentHome, agentPath)
	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd /usr/local/bin/sp --args %s --env %s --duration 10s",
		AgentServiceName,
		fmt.Sprintf("hub,agent,--hub-url,%s,--sprite,%s", opts.HubURL, opts.Sprite),
		shellQuote(env),
	)
	if out, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  opts.Sprite,
		Org:     a.org,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating agent service on %s: %w\n%s", opts.Sprite, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// InstallAgentOptions configures installing the reporting agent on a sprite.
type InstallAgentOptions struct {
	// Sprite is the sprite to install the agent on.
	Sprite string
	// HubURL is the base URL the agent reports to.
	HubURL string
	// Token is the Sprites org token the agent authenticates with.
	Token string
	// BinaryPath is the local path to a gzipped linux/amd64 sp binary.
	BinaryPath string
}

// AgentServiceName is the sprite-env service name for the reporting agent.
const AgentServiceName = "sp-hub-agent"

// RemoveAgent deletes the reporting agent service from a sprite.
func (a *Actions) RemoveAgent(spriteName string) error {
	if _, err := a.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     a.org,
		Command: []string{"sprite-env", "services", "delete", AgentServiceName},
	}); err != nil {
		return fmt.Errorf("removing agent service from %s: %w", spriteName, err)
	}
	return nil
}

// List returns every sprite in the organization, from the single org listing
// call. This is the only sprite API call the page makes on load: anything that
// asks an individual sprite a question would wake it.
func (a *Actions) List() ([]sprite.Info, error) {
	infos, err := a.client.List()
	if err != nil {
		return nil, fmt.Errorf("listing sprites: %w", err)
	}
	return infos, nil
}

// sessionIDPattern matches the UUID filenames Claude uses for transcripts.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// validateSessionID rejects anything that isn't a plain session identifier,
// since the value is interpolated into a shell command.
func validateSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

// validatePath rejects paths that aren't plain absolute paths. Directories
// reach here from agent reports and from the query string, and end up inside a
// shell command, so they are checked rather than trusted.
func validatePath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\n\r\x00") {
		return fmt.Errorf("invalid directory %q", p)
	}
	return nil
}

// validateURL rejects anything that isn't a plain https URL.
func validateURL(u string) error {
	if !strings.HasPrefix(u, "https://") || strings.ContainsAny(u, " \n\r\x00'\"") {
		return fmt.Errorf("invalid hub URL %q", u)
	}
	return nil
}

// shellQuote wraps a value in single quotes for safe use in a shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
