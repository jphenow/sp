package setup

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jphenow/sp/internal/sprite"
)

// TokenProvider manages Claude Code OAuth tokens for sprite authentication.
type TokenProvider struct {
	tokenPath string
}

// NewTokenProvider creates a provider that reads/writes tokens from the default path.
func NewTokenProvider() *TokenProvider {
	home, _ := os.UserHomeDir()
	return &TokenProvider{
		tokenPath: filepath.Join(home, ".claude-token"),
	}
}

// GetToken returns the Claude Code OAuth token from env or file.
// If no token is available, it prompts the user interactively.
func (tp *TokenProvider) GetToken() (string, error) {
	// Check environment variable first
	if token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		return token, nil
	}

	// Check token file
	data, err := os.ReadFile(tp.tokenPath)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if isValidToken(token) {
			return token, nil
		}
	}

	// Interactive prompt
	return tp.promptForToken()
}

// promptForToken asks the user to provide a Claude Code OAuth token.
func (tp *TokenProvider) promptForToken() (string, error) {
	fmt.Println("No Claude Code OAuth token found.")
	fmt.Println("Run 'claude setup-token' in another terminal to get one, then paste it here.")
	fmt.Print("Token: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", fmt.Errorf("reading token: %w", scanner.Err())
	}

	token := strings.TrimSpace(scanner.Text())
	if !isValidToken(token) {
		return "", fmt.Errorf("invalid token format: must start with 'sk-ant-oat01-' and be at least 100 characters")
	}

	// Save for future use
	if err := os.WriteFile(tp.tokenPath, []byte(token), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save token: %v\n", err)
	}

	return token, nil
}

// isValidToken checks if a token has the expected format.
func isValidToken(token string) bool {
	return strings.HasPrefix(token, "sk-ant-oat01-") && len(token) >= 100
}

// LocalClaudeCredentials returns the raw Claude Code OAuth credentials
// JSON from the user's local machine, or nil if no credentials are
// available. On macOS the credentials live in Keychain (service name
// "Claude Code-credentials"); on Linux they live in a plain file at
// ~/.claude/.credentials.json. Both paths produce the same JSON shape:
//
//	{"claudeAiOauth":{"accessToken":"...","refreshToken":"...",
//	                   "expiresAt":<int>,"scopes":[...],
//	                   "subscriptionType":"...","rateLimitTier":"..."}}
//
// The refreshToken is the durable part: pushed to a sprite, Claude Code
// on the sprite can auto-refresh the access token without user
// interaction, which is strictly better than the `claude setup-token`
// path (those tokens have tight quota limits and expire within a day).
//
// Returns nil on any error — missing keychain entry, missing file,
// permission denied, etc. The caller should treat nil as "fall back to
// the legacy CLAUDE_CODE_OAUTH_TOKEN path" rather than "fatal error".
func LocalClaudeCredentials() []byte {
	switch runtime.GOOS {
	case "darwin":
		return readClaudeCredentialsKeychain()
	case "linux":
		return readClaudeCredentialsFile()
	default:
		return nil
	}
}

// readClaudeCredentialsKeychain pulls the credentials blob out of the
// macOS Keychain. The Keychain entry is created by Claude Code on login
// and updated whenever the access token refreshes, so reading it on
// each connect picks up the freshest credentials automatically.
//
// Note: the first time `security` reads the entry for a process it may
// prompt the user for Keychain access. Subsequent reads from the same
// binary can be allowlisted ("Always Allow"). If the user denies or
// cancels the prompt, security exits non-zero and we return nil.
func readClaudeCredentialsKeychain() []byte {
	u, err := user.Current()
	if err != nil {
		return nil
	}
	cmd := exec.Command("security",
		"find-generic-password",
		"-s", "Claude Code-credentials",
		"-a", u.Username,
		"-w")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return bytes.TrimSpace(out)
}

// readClaudeCredentialsFile reads the credentials file that Claude Code
// writes on Linux. Path is fixed at ~/.claude/.credentials.json.
func readClaudeCredentialsFile() []byte {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return nil
	}
	return data
}

// EnsureSpriteClaudeSettings merges bypass-permissions defaults into the
// sprite's ~/.claude/settings.json. This is sprite-scoped — the user's
// local settings.json is never modified. PushClaudeConfig runs first
// and drops the local settings.json onto the sprite verbatim; this
// function then augments it with sprite-specific bypass defaults.
//
// Two fields are merged under `permissions`:
//
//   - defaultMode = "bypassPermissions": makes `claude` default to
//     bypass mode without needing the --dangerously-skip-permissions
//     flag. Sprites are disposable environments where per-tool approval
//     prompts are friction, not safety.
//
//   - skipDangerousModePermissionPrompt = true: pre-accepts the
//     one-time "Bypass Permissions mode" consent dialog so claude
//     starts straight into the session instead of asking to confirm.
//
// Idempotent: re-running just re-writes the same keys. Uses python3
// for JSON-safe merging — sed/awk would be fragile against arbitrary
// pre-existing settings.json structures. python3 is on essentially
// every modern Linux sprite image.
func EnsureSpriteClaudeSettings(client *sprite.Client, spriteName string) error {
	home, _ := os.UserHomeDir()

	// The python script is embedded as a here-doc to keep the JSON
	// manipulation logic readable and to avoid any shell-level escaping
	// headaches around the nested quotes in dict key access.
	//
	// It does three things:
	//   1. Merges bypass-permissions defaults (defaultMode + skipPrompt).
	//   2. Rewrites any statusLine.command path that references the user's
	//      LOCAL home directory to /home/sprite — PushClaudeConfig copies
	//      settings.json verbatim from local, so statusLine paths like
	//      "/Users/jon/.claude/statusline-command.sh" wouldn't exist on
	//      the sprite without this fixup.
	//   3. Writes the result back with indent for readability.
	script := fmt.Sprintf(`mkdir -p ~/.claude
python3 <<'PYEOF'
import json, os, re
p = os.path.expanduser("~/.claude/settings.json")
try:
    with open(p) as f:
        d = json.load(f)
    if not isinstance(d, dict):
        d = {}
except Exception:
    d = {}

# 1. Bypass permissions
perm = d.setdefault("permissions", {})
if not isinstance(perm, dict):
    perm = {}
    d["permissions"] = perm
perm["defaultMode"] = "bypassPermissions"
perm["skipDangerousModePermissionPrompt"] = True

# 2. Fix paths referencing local home dir — replace with sprite home.
#    Applies to statusLine.command and any other string values that embed
#    the local user's home directory.
LOCAL_HOME = %q
sl = d.get("statusLine")
if isinstance(sl, dict) and "command" in sl:
    sl["command"] = sl["command"].replace(LOCAL_HOME, "/home/sprite")

with open(p, "w") as f:
    json.dump(d, f, indent=2)

# 3. Rewrite all local home paths in plugin JSON files. Claude uses these
#    paths to locate cached plugin code and marketplace repos; without
#    the rewrite, plugins installed locally are "not found" on the sprite.
plugin_files = [
    ("~/.claude/plugins/installed_plugins.json", True),
    ("~/.claude/plugins/known_marketplaces.json", True),
]
for pf, deep in plugin_files:
    fp = os.path.expanduser(pf)
    try:
        with open(fp) as f:
            raw = f.read()
        if LOCAL_HOME in raw:
            raw = raw.replace(LOCAL_HOME, "/home/sprite")
            with open(fp, "w") as f:
                f.write(raw)
    except Exception:
        pass  # best-effort; missing file is fine
PYEOF
`, home)
	_, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", script},
	})
	return err
}

// PushClaudeCredentials writes the raw Claude OAuth credentials JSON to
// ~/.claude/.credentials.json on the sprite with 600 perms. Meant to be
// called with the output of LocalClaudeCredentials; caller decides
// whether the bytes are worth pushing.
//
// Implementation note: the credentials are base64-encoded into a shell
// command and decoded on the sprite via `base64 -d`, NOT uploaded via
// `sprite exec -file`. The reason is that the -file upload path on the
// sprite runtime creates files (and any auto-created parent directories)
// owned by `ubuntu:ubuntu`, not the sprite user. With ~/.claude ending
// up as ubuntu:700, the sprite user can't even traverse it, so claude
// gets "Permission denied" when trying to read its own credentials and
// silently falls back to an interactive login. Shell-redirect writes
// run as the sprite user and produce sprite-owned files, sidestepping
// the entire ownership tangle.
//
// After this runs, the sprite's claude will read credentials from the
// file (position 6 in the auth precedence hierarchy) and use the
// embedded refreshToken to auto-refresh the accessToken as needed.
// CLAUDE_CODE_OAUTH_TOKEN should NOT be set in the same session —
// it sits at position 5 and would mask the file, preventing refresh
// when the access token in the env var goes stale.
func PushClaudeCredentials(client *sprite.Client, spriteName string, creds []byte) error {
	if len(creds) == 0 {
		return fmt.Errorf("empty credentials blob")
	}
	// Base64 in standard encoding has no shell-special characters, so
	// it's safe to interpolate into a sh -c command without quoting.
	encoded := base64.StdEncoding.EncodeToString(creds)
	script := fmt.Sprintf(`
mkdir -p ~/.claude
chmod 700 ~/.claude
printf '%%s' '%s' | base64 -d > ~/.claude/.credentials.json
chmod 600 ~/.claude/.credentials.json
`, encoded)
	_, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", script},
	})
	return err
}

// LocalGhToken returns the user's active gh OAuth token from their local
// gh installation, or the empty string if gh is not installed or not
// authenticated. `gh auth token` prints the active token regardless of
// whether gh stores it in a file (Linux) or keyring (macOS), which is
// critical: on macOS the ~/.config/gh/hosts.yml file has no oauth_token
// field at all — the token lives in the Keychain. Copying hosts.yml
// alone is useless there.
func LocalGhToken() string {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SetupGhAuth plumbs gh authentication into the sprite. Two parts:
//
//  1. Exports GH_TOKEN to .bashrc/.zshrc/.profile in an idempotent
//     marker block. gh always honors GH_TOKEN ahead of any file-based
//     or keyring-based lookup, so this bypasses the "token in hosts.yml
//     is invalid" problem that arises from macOS Keychain storage not
//     making the token visible in the YAML file we could copy.
//
//  2. Pushes ~/.config/gh/config.yml (user preferences — editor,
//     aliases, prompt style) to the sprite. config.yml is non-sensitive
//     and small (~1KB). hosts.yml is deliberately NOT pushed because
//     on macOS it's incomplete (no token) and misleading to gh's
//     auth lookup on the sprite.
//
// Silent no-op if the local machine has no gh token (user never ran
// `gh auth login`). Non-fatal on upload errors so a transient failure
// doesn't block the whole connect flow.
func SetupGhAuth(client *sprite.Client, spriteName string) error {
	token := LocalGhToken()
	if token == "" {
		// No local gh auth to propagate. Not an error.
		return nil
	}

	// Clean up any prior GH_TOKEN exports from rc files left by older sp
	// versions. We no longer write tokens to disk — they're injected via
	// sprite exec -env + tmux setenv -g on every connect, so they only
	// exist in running processes and vanish when the sprite cold-stops.
	// This avoids leaving a valid GitHub token in plaintext on a dormant
	// sprite's filesystem.
	cleanScript := `
for RC in .bashrc .zshrc .profile; do
    sed -i '/# sp-gh-auth-begin/,/# sp-gh-auth-end/d' ~/$RC 2>/dev/null || true
done
`
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", cleanScript},
	}); err != nil {
		// Non-fatal: cleanup of old state shouldn't block connect.
		_ = err
	}

	// Push config.yml (preferences) if it exists. Non-critical — gh
	// works fine without it, just uses defaults.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil // gracefully skip the file push, token export already succeeded
	}
	configPath := filepath.Join(home, ".config", "gh", "config.yml")
	if _, err := os.Stat(configPath); err != nil {
		return nil
	}
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", "mkdir -p ~/.config/gh && chmod 700 ~/.config/gh && chmod 600 ~/.config/gh/config.yml 2>/dev/null || true"},
		Files:   map[string]string{configPath: "/home/sprite/.config/gh/config.yml"},
	}); err != nil {
		return fmt.Errorf("uploading gh config.yml: %w", err)
	}
	return nil
}

// FixSpriteHomePermissions papers over a base-image quirk where /home/sprite
// ships as ubuntu:ubuntu 0750, and additionally cleans up any ubuntu-owned
// state under /home/sprite/.claude that earlier sp versions may have left
// behind via the sprite-exec -file upload path (which creates files as
// ubuntu, not sprite — see PushClaudeCredentials for the gory details).
//
// Normal file ops on /home/sprite work through some runtime override (ACL
// or similar), but git's stricter path-walk refuses to operate inside a
// home dir it sees as foreign-owned (symptom: `git init` fails with
// "Cannot access work tree: Permission denied"). chown'ing to
// sprite:sprite 0755 via sudo makes git work again.
//
// For ~/.claude specifically, claude reads its credentials and settings
// from there as the sprite user — if any of that state ends up owned by
// ubuntu (mode 700), claude gets EACCES and silently falls back to an
// interactive login. We chown -R it on every connect to keep it sane.
//
// All chowns are idempotent and best-effort: failures (no sudo, dir
// doesn't exist) are silently swallowed.
func FixSpriteHomePermissions(client *sprite.Client, spriteName string) error {
	script := `
sudo -n chown sprite:sprite /home/sprite 2>/dev/null || true
sudo -n chmod 755 /home/sprite 2>/dev/null || true
if [ -e /home/sprite/.claude ]; then
  sudo -n chown -R sprite:sprite /home/sprite/.claude 2>/dev/null || true
fi
`
	_, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", script},
	})
	return err
}

// SetupSpriteAuth provisions SSH keys, Claude token, and shell config on a
// sprite in as few exec calls as possible to minimize websocket overhead.
//
// Phase 1: upload SSH keys (needs -file flag, so one exec per file).
// Phase 2: one mega-script exec that writes all rc files, claude.json, and
//
//	SSH config in a single shell invocation. This is the key
//	optimization: prior versions did 8+ separate execs at ~5-10s each.
//
// If token is empty, the CLAUDE_CODE_OAUTH_TOKEN export is skipped — the
// caller has already pushed a credentials.json via PushClaudeCredentials.
func SetupSpriteAuth(client *sprite.Client, spriteName, token string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	sshKeyPath := filepath.Join(home, ".ssh", "id_ed25519")
	sshPubPath := sshKeyPath + ".pub"

	// --- Phase 1: upload SSH keys (need -file flag, so separate execs) ---
	// Combine private + public key into one exec where possible.
	files := map[string]string{}
	if _, err := os.Stat(sshKeyPath); err == nil {
		files[sshKeyPath] = "/home/sprite/.ssh/id_ed25519"
	}
	if _, err := os.Stat(sshPubPath); err == nil {
		files[sshPubPath] = "/home/sprite/.ssh/id_ed25519.pub"
	}
	if len(files) > 0 {
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", "mkdir -p ~/.ssh && chmod 700 ~/.ssh && chmod 600 ~/.ssh/id_ed25519 2>/dev/null; chmod 644 ~/.ssh/id_ed25519.pub 2>/dev/null; true"},
			Files:   files,
		}); err != nil {
			return fmt.Errorf("uploading SSH keys: %w", err)
		}
	}

	// --- Phase 2: one mega-script for all shell writes ---
	// Build the auth block content. The block is written to all 3 rc files.
	exportLine := ""
	if token != "" {
		quotedToken := "'" + strings.ReplaceAll(token, "'", `'\''`) + "'"
		exportLine = "export CLAUDE_CODE_OAUTH_TOKEN=" + quotedToken + "\n"
	}
	authBlock := fmt.Sprintf(`# sp-claude-auth-begin
unset ANTHROPIC_API_KEY
unset ANTHROPIC_AUTH_TOKEN
unset CLAUDE_CODE_USE_BEDROCK
unset CLAUDE_CODE_USE_VERTEX
unset CLAUDE_CODE_USE_FOUNDRY
%salias claude='command claude --dangerously-skip-permissions'
# sp-claude-auth-end`, exportLine)

	claudeConfig := `{"bypassPermissionsModeAccepted":true,"hasCompletedOnboarding":true}`

	// The mega-script handles: rc-file auth blocks (3 files), claude.json,
	// and SSH config — all in one exec. Single-digit seconds instead of
	// 8 × 5-10s = 40-80s.
	megaScript := fmt.Sprintf(`
set -e
# --- Auth block for .bashrc, .zshrc, .profile ---
for RC in .bashrc .zshrc .profile; do
    touch ~/$RC
    sed -i '/# sp-claude-auth-begin/,/# sp-claude-auth-end/d' ~/$RC 2>/dev/null || true
    sed -i '/CLAUDE_CODE_OAUTH_TOKEN/d' ~/$RC 2>/dev/null || true
    cat >> ~/$RC <<'AUTHEOF'
%s
AUTHEOF
done

# --- claude.json onboarding bypass ---
mkdir -p ~/.claude
echo '%s' > ~/.claude/claude.json

# --- SSH config for GitHub ---
mkdir -p ~/.ssh && chmod 700 ~/.ssh
touch ~/.ssh/config
sed -i '/# sp-managed-github-begin/,/# sp-managed-github-end/d' ~/.ssh/config 2>/dev/null || true
cat >> ~/.ssh/config <<'SSHEOF'
# sp-managed-github-begin
Host github.com
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
# sp-managed-github-end
SSHEOF
chmod 600 ~/.ssh/config
`, authBlock, claudeConfig)

	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", megaScript},
	}); err != nil {
		return fmt.Errorf("running auth mega-script: %w", err)
	}

	return nil
}

// InstallOpenWrapper installs a ~/bin/open script inside the sprite that emits
// the OSC 9999 browser-open escape sequence. When running inside tmux, the
// script wraps the sequence in a DCS passthrough envelope so the local sprite
// client can intercept it. Without this, tmux swallows unknown OSC sequences.
func InstallOpenWrapper(client *sprite.Client, spriteName string) error {
	// The open wrapper script:
	// - Detects $TMUX to determine if we need DCS passthrough wrapping
	// - Emits OSC 9999;browser-open;<url> BEL, optionally inside DCS tmux envelope
	script := `#!/bin/sh
# sp-managed open wrapper — emits OSC 9999 browser-open for the sprite client.
# When running inside tmux, wraps in DCS passthrough envelope.
url="$1"
if [ -z "$url" ]; then
  echo "Usage: open <url>" >&2
  exit 1
fi

if [ -n "$TMUX" ]; then
  # DCS passthrough: ESC P tmux; <inner with ESC doubled> ESC backslash
  printf '\033Ptmux;\033\033]9999;browser-open;%s\007\033\\' "$url"
else
  printf '\033]9999;browser-open;%s\007' "$url"
fi
`

	// Install to ~/bin/open with PATH addition
	installCmd := `
		mkdir -p ~/bin
		cat > ~/bin/open << 'OPENEOF'
` + script + `OPENEOF
		chmod +x ~/bin/open
		# Ensure ~/bin is in PATH via .bashrc (idempotent)
		grep -q 'export PATH="$HOME/bin:$PATH"' ~/.bashrc 2>/dev/null || echo 'export PATH="$HOME/bin:$PATH"' >> ~/.bashrc
	`

	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", installCmd},
	}); err != nil {
		return fmt.Errorf("installing open wrapper: %w", err)
	}

	return nil
}

// SetupGitConfig copies the local user's git config (name, email) to the
// sprite and configures git to use HTTPS + GH_TOKEN for GitHub operations
// instead of SSH. This avoids passphrase prompts when SSH keys are
// password-protected (increasingly required by company policy).
//
// The setup does three things in a single exec call:
//
//  1. Sets user.name and user.email from the local git config.
//
//  2. Rewrites SSH GitHub URLs to HTTPS: any `git@github.com:<path>` URL
//     is transparently converted to `https://github.com/<path>`. This
//     covers repos cloned with SSH remotes that get synced into the sprite
//     via mutagen. Also removes any prior HTTPS→SSH rewrite from older sp
//     versions so the two don't conflict.
//
//  3. Installs a credential helper that reads GH_TOKEN at runtime (from
//     the environment, set by SetupGhAuth on every connect). Git calls
//     this helper every time it needs credentials for github.com, so
//     token refreshes between connects are picked up automatically. No
//     SSH key involvement, no passphrase prompt, works non-interactively
//     for claude's git operations.
func SetupGitConfig(client *sprite.Client, spriteName string) error {
	name, _ := gitConfigValue("user.name")
	email, _ := gitConfigValue("user.email")

	// Build a single script for all git config operations. Was 3 separate
	// exec calls before; consolidated to 1 for speed (~10s saved).
	script := "set -e\n"
	if name != "" {
		script += fmt.Sprintf("git config --global user.name %s\n", shellQuote(name))
	}
	if email != "" {
		script += fmt.Sprintf("git config --global user.email %s\n", shellQuote(email))
	}

	// Remove any prior HTTPS→SSH rewrite (from older sp versions) that
	// would conflict with our SSH→HTTPS rewrite.
	script += `
git config --global --unset-all 'url.git@github.com:.insteadOf' 2>/dev/null || true
`

	// SSH→HTTPS rewrite: git@github.com: → https://github.com/
	script += `
git config --global 'url.https://github.com/.insteadOf' 'git@github.com:'
`

	// Credential helper: reads GH_TOKEN from env at runtime. The helper
	// is a shell function that git invokes when it needs auth for
	// github.com. Because it reads $GH_TOKEN dynamically, token updates
	// via SetupGhAuth are picked up without restarting git.
	script += `
git config --global credential.helper '!f() { echo "protocol=https"; echo "host=github.com"; echo "username=x-access-token"; echo "password=$GH_TOKEN"; }; f'
`

	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", script},
	}); err != nil {
		return fmt.Errorf("setting up git config: %w", err)
	}
	return nil
}

// shellQuote wraps a string in single quotes for safe shell interpolation.
// This is a package-level helper duplicated from cmd/ because setup/ can't
// import cmd/ (would create a cycle). Identical implementation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitConfigValue reads a git config value from the local machine.
func gitConfigValue(key string) (string, error) {
	out, err := runGitCommand("config", "--get", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
