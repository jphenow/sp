package setup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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

// SetupSpriteAuth provisions SSH keys, Claude token, and shell config on a sprite.
// This mirrors the Bash script's setup_sprite_auth function.
func SetupSpriteAuth(client *sprite.Client, spriteName, token string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	sshKeyPath := filepath.Join(home, ".ssh", "id_ed25519")
	sshPubPath := sshKeyPath + ".pub"

	// Write Claude token to shell RC files
	tokenExport := fmt.Sprintf(`export CLAUDE_CODE_OAUTH_TOKEN="%s"`, token)
	for _, rcFile := range []string{".bashrc", ".zshrc"} {
		cmd := fmt.Sprintf(`grep -q CLAUDE_CODE_OAUTH_TOKEN ~/%s 2>/dev/null && sed -i '/CLAUDE_CODE_OAUTH_TOKEN/d' ~/%s; echo '%s' >> ~/%s`,
			rcFile, rcFile, tokenExport, rcFile)
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", cmd},
		}); err != nil {
			return fmt.Errorf("writing token to %s: %w", rcFile, err)
		}
	}

	// Write Claude config for onboarding bypass
	claudeConfig := `{"bypassPermissionsModeAccepted":true,"hasCompletedOnboarding":true}`
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", fmt.Sprintf(`mkdir -p ~/.claude && echo '%s' > ~/.claude/claude.json`, claudeConfig)},
	}); err != nil {
		return fmt.Errorf("writing claude config: %w", err)
	}

	// Upload SSH keys
	if _, err := os.Stat(sshKeyPath); err == nil {
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", "mkdir -p ~/.ssh && chmod 700 ~/.ssh"},
			Files:   map[string]string{sshKeyPath: "/home/sprite/.ssh/id_ed25519"},
		}); err != nil {
			return fmt.Errorf("uploading SSH private key: %w", err)
		}
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", "chmod 600 ~/.ssh/id_ed25519"},
		}); err != nil {
			return fmt.Errorf("setting SSH key permissions: %w", err)
		}
	}

	if _, err := os.Stat(sshPubPath); err == nil {
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", "true"},
			Files:   map[string]string{sshPubPath: "/home/sprite/.ssh/id_ed25519.pub"},
		}); err != nil {
			return fmt.Errorf("uploading SSH public key: %w", err)
		}
	}

	// Write SSH config for GitHub
	sshConfig := `Host github.com
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
`
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName,
		Command: []string{"sh", "-c", fmt.Sprintf(`cat >> ~/.ssh/config << 'SSHEOF'
%s
SSHEOF
chmod 600 ~/.ssh/config`, sshConfig)},
	}); err != nil {
		return fmt.Errorf("writing SSH config: %w", err)
	}

	return nil
}

// SetupGitConfig copies the local user's git config (name, email) to the sprite.
func SetupGitConfig(client *sprite.Client, spriteName string) error {
	name, _ := gitConfigValue("user.name")
	email, _ := gitConfigValue("user.email")

	if name != "" {
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"git", "config", "--global", "user.name", name},
		}); err != nil {
			return fmt.Errorf("setting git user.name: %w", err)
		}
	}

	if email != "" {
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"git", "config", "--global", "user.email", email},
		}); err != nil {
			return fmt.Errorf("setting git user.email: %w", err)
		}
	}

	return nil
}

// gitConfigValue reads a git config value from the local machine.
func gitConfigValue(key string) (string, error) {
	out, err := runGitCommand("config", "--get", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
