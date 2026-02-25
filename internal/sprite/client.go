package sprite

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

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

	out, err := exec.Command("sprite", args...).Output()
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

	out, err := exec.Command("sprite", args...).Output()
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
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating sprite %q: %w\n%s", name, err, string(out))
	}
	return nil
}

// Destroy destroys a sprite by name.
func (c *Client) Destroy(name string) error {
	args := []string{"destroy"}
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
// For interactive (TTY) sessions, use ExecInteractive instead.
func (c *Client) Exec(opts ExecOptions) ([]byte, error) {
	args := c.BuildExecArgs(opts)
	cmd := exec.Command("sprite", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("exec on sprite %q: %w\n%s", opts.Sprite, err, string(out))
	}
	return out, nil
}

// ExecInteractive runs an interactive command on a sprite with TTY attached.
// This replaces the current process's stdin/stdout/stderr.
func (c *Client) ExecInteractive(opts ExecOptions) error {
	opts.TTY = true
	args := c.BuildExecArgs(opts)

	cmd := exec.Command("sprite", args...)
	cmd.Stdin = nil // will be set by caller or syscall.Exec
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
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
		args = append(args, "-tty")
	}
	if opts.Dir != "" {
		args = append(args, "-dir", opts.Dir)
	}
	if opts.Detach {
		args = append(args, "-detach")
	}
	if len(opts.Env) > 0 {
		var envParts []string
		for k, v := range opts.Env {
			envParts = append(envParts, k+"="+v)
		}
		args = append(args, "-env", strings.Join(envParts, ","))
	}
	for local, remote := range opts.Files {
		args = append(args, "-file", local+":"+remote)
	}
	args = append(args, opts.Command...)
	return args
}

// StartProxy starts a sprite proxy for port forwarding and returns the command
// (which runs as a background process). Caller is responsible for managing the process.
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
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting proxy for sprite %q: %w", opts.Sprite, err)
	}
	return cmd, nil
}

// GetURL returns the public URL for a sprite.
func (c *Client) GetURL(name string) (string, error) {
	args := []string{"url"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, "-s", name)

	out, err := exec.Command("sprite", args...).Output()
	if err != nil {
		return "", fmt.Errorf("getting URL for sprite %q: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Exists checks if a sprite with the given name exists by attempting to get it.
func (c *Client) Exists(name string) (bool, error) {
	info, err := c.Get(name)
	if err != nil {
		// If we get an error, the sprite might not exist or there's a network issue.
		// Check the sprite list as fallback.
		sprites, listErr := c.List()
		if listErr != nil {
			return false, fmt.Errorf("checking sprite existence: %w", err)
		}
		for _, s := range sprites {
			if s.Name == name {
				return true, nil
			}
		}
		return false, nil
	}
	return info != nil, nil
}

// Use associates the current directory with a sprite name (creates .sprite file).
func (c *Client) Use(name string) error {
	args := []string{"use"}
	if c.org != "" {
		args = append(args, "-o", c.org)
	}
	args = append(args, name)

	cmd := exec.Command("sprite", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sprite use %q: %w\n%s", name, err, string(out))
	}
	return nil
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
