package sprite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// runCapture runs a sprite CLI command, returning stdout and — on failure — an
// error carrying the tail of stderr.
//
// The API commands parse JSON from stdout, so CombinedOutput isn't an option;
// but plain .Output() drops stderr entirely, which is where the sprite CLI puts
// the only useful part of a failure. That's how a platform outage surfaced as
// the bare, undiagnosable "exit status 35" instead of
// "curl: (35) Recv failure: Connection reset by peer".
func runCapture(args ...string) ([]byte, error) {
	cmd := exec.Command("sprite", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := lastMeaningfulLine(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
		return out, err
	}
	return out, nil
}

// lastMeaningfulLine picks the final non-empty line of stderr, which for the
// sprite CLI is the actual failure (curl progress meters and "Calling API:"
// banners precede it).
func lastMeaningfulLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// Client provides access to the Sprites API and CLI.
type Client struct {
	org string // default organization
}

// NewClient creates a new sprite client with an optional default organization.
func NewClient(org string) *Client {
	return &Client{org: org}
}

// List returns all sprites visible to the current user from the Sprites API.
func (c *Client) List() ([]Info, error) {
	args := []string{"api"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, "/sprites")

	start := time.Now()
	out, err := runCapture(args...)
	record("api", "/sprites", start, err)
	if err != nil {
		return nil, fmt.Errorf("listing sprites: %w", err)
	}

	var resp ListResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parsing sprite list: %w", err)
	}
	return resp.Sprites, nil
}

// Get returns a single sprite's info by name from the Sprites API.
func (c *Client) Get(name string) (*Info, error) {
	args := []string{"api"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, "-s", name, "/")

	start := time.Now()
	out, err := runCapture(args...)
	record("api", name+": info", start, err)
	if err != nil {
		return nil, fmt.Errorf("getting sprite %q: %w", name, err)
	}

	var info Info
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parsing sprite info: %w", err)
	}
	return &info, nil
}

// Create creates a new sprite with the given name. Returns once the sprite exists
// but does not wait for it to be fully ready.
func (c *Client) Create(name string) error {
	args := []string{"create", "-skip-console"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, name)

	cmd := exec.Command("sprite", args...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	record("create", name, start, err)
	if err != nil {
		return fmt.Errorf("creating sprite %q: %w\n%s", name, err, string(out))
	}
	return nil
}

// Destroy destroys a sprite by name. Always passes --force to skip the
// interactive confirmation prompt — callers are expected to do their own
// confirmation up-front (e.g. sp rm's safety rails, sp prune's dry-run).
// Without --force, sprite destroy blocks on a tty prompt that CombinedOutput
// never answers, so the caller hangs indefinitely.
func (c *Client) Destroy(name string) error {
	args := []string{"destroy", "--force"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, name)

	cmd := exec.Command("sprite", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("destroying sprite %q: %w\n%s", name, err, string(out))
	}
	return nil
}

// Exec runs a command on a sprite and returns its combined output.
// Interactive (TTY) sessions build their args with BuildExecArgs instead.
func (c *Client) Exec(opts ExecOptions) ([]byte, error) {
	args := c.BuildExecArgs(opts)
	// --debug is a global flag, so it precedes the subcommand. It writes to the
	// named file and leaves stdout clean (verified), so CombinedOutput is
	// unaffected. The log is analyzed only if this call turns out slow.
	debugFlag, finishDebug := withDebugLog()
	if debugFlag != "" {
		args = append([]string{debugFlag}, args...)
	}
	cmd := exec.Command("sprite", args...)
	start := time.Now()
	detail := describeExec(opts)
	id := beginCall(detail, start)
	out, err := cmd.CombinedOutput()
	endCall(id)
	dur := time.Since(start)
	if gap := finishDebug(dur); gap != "" {
		detail += " [" + gap + "]"
	}
	record("exec", detail, start, err)
	if err != nil {
		return out, fmt.Errorf("exec on sprite %q: %w\n%s", opts.Sprite, err, string(out))
	}
	return out, nil
}

// BuildExecArgs constructs the argument list for sprite exec.
// Exported for use by the connect command which needs the raw args for process replacement.
func (c *Client) BuildExecArgs(opts ExecOptions) []string {
	args := []string{"exec"}
	org := opts.Org
	if org == "" {
		org = c.org
	}
	if org != "" {
		args = append(args, "-o", org)
	}
	if opts.Sprite != "" {
		args = append(args, "-s", opts.Sprite)
	}
	if opts.TTY {
		args = append(args, "--tty")
	}
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	if opts.Detach {
		args = append(args, "-detach")
	}
	if len(opts.Env) > 0 {
		var envParts []string
		for k, v := range opts.Env {
			envParts = append(envParts, k+"="+v)
		}
		args = append(args, "--env", strings.Join(envParts, ","))
	}
	// Multi-char flags must use the double-dash long form: the sprite CLI's
	// flag parser treats a single-dash multi-char token as grouped
	// shorthands (e.g. `-file` -> `-f -i -l -e`), which fails on the second
	// occurrence with "unknown shorthand flag: 'f'". Only -o/-s are real
	// single-char shorthands.
	for local, remote := range opts.Files {
		args = append(args, "--file", local+":"+remote)
	}
	// The sprite CLI requires a `--` separator before the command so that
	// command flags (e.g. `sh -c`) aren't parsed as sprite's own flags.
	// Without it, `sprite exec -s <name> sh -c '...'` fails with
	// "unknown shorthand flag: 'c'" and exit status 2.
	if len(opts.Command) > 0 {
		args = append(args, "--")
		args = append(args, opts.Command...)
	}
	return args
}

// StartProxy starts a sprite proxy for port forwarding and returns the command
// (which runs as a background process). Caller is responsible for managing the process.
// Stderr is captured in a buffer so callers can read proxy error output if it exits.
func (c *Client) StartProxy(opts ProxyOptions) (*exec.Cmd, error) {
	args := []string{"proxy"}
	org := opts.Org
	if org == "" {
		org = c.org
	}
	if org != "" {
		args = append(args, "-o", org)
	}
	if opts.Sprite != "" {
		args = append(args, "-s", opts.Sprite)
	}
	args = append(args, opts.Ports...)

	cmd := exec.Command("sprite", args...)
	// Capture stderr so we can diagnose proxy failures
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting proxy for sprite %q: %w", opts.Sprite, err)
	}
	return cmd, nil
}

// ProxyStderr returns the captured stderr from a proxy command, if available.
func ProxyStderr(cmd *exec.Cmd) string {
	if cmd == nil || cmd.Stderr == nil {
		return ""
	}
	if buf, ok := cmd.Stderr.(*bytes.Buffer); ok {
		return buf.String()
	}
	return ""
}

// Exists checks if a sprite with the given name exists by attempting to get it.
// Returns true only when the API returns a sprite with a non-empty ID.
func (c *Client) Exists(name string) (bool, error) {
	_, ok, err := c.ExistsInfo(name)
	return ok, err
}

// ExistsInfo is Exists plus the Info it already had to fetch to answer. Callers
// that want the sprite's Status (running / warm / cold) should use this rather
// than a second Get — the whole reason to know the status is that a cold sprite
// makes every subsequent call expensive, so paying an extra round trip to learn
// it would be self-defeating. Info is nil when the answer came from the list
// fallback or the sprite doesn't exist.
func (c *Client) ExistsInfo(name string) (*Info, bool, error) {
	info, err := c.Get(name)
	if err != nil {
		// If we get an error, the sprite might not exist or there's a network issue.
		// Check the sprite list as fallback.
		sprites, listErr := c.List()
		if listErr != nil {
			return nil, false, fmt.Errorf("checking sprite existence: %w", err)
		}
		for _, s := range sprites {
			if s.Name == name {
				found := s
				return &found, true, nil
			}
		}
		return nil, false, nil
	}
	// Guard against the API returning an empty/null JSON body that
	// deserialises into a zero-value Info struct.
	if info == nil || info.ID == "" {
		return nil, false, nil
	}
	return info, true, nil
}

// Sessions lists active tmux/exec sessions on a sprite.
func (c *Client) Sessions(name string) (string, error) {
	args := []string{"sessions", "list"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, "-s", name)

	out, err := exec.Command("sprite", args...).Output()
	if err != nil {
		return "", fmt.Errorf("listing sessions for sprite %q: %w", name, err)
	}
	return string(out), nil
}
