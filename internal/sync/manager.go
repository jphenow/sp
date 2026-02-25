package sync

import (
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jphenow/sp/internal/sprite"
)

// Manager handles Mutagen sync session lifecycle for sprite environments.
// It manages SSH server setup, proxy processes, and Mutagen sessions.
type Manager struct {
	client *sprite.Client
}

// SessionState describes the current state of a sync session.
type SessionState struct {
	Name           string
	MutagenID      string
	Status         string // "watching", "syncing", "connecting", "error", "none"
	AlphaConnected bool
	BetaConnected  bool
	Conflicts      int
	LastError      string
	SSHPort        int
	ProxyPID       int
}

// NewManager creates a new sync manager wrapping the given sprite client.
func NewManager(client *sprite.Client) *Manager {
	return &Manager{client: client}
}

// ComputeSSHPort derives a deterministic SSH port from a sprite name.
// Port range: 10000-60000 to avoid privileged ports and common service ports.
func ComputeSSHPort(spriteName string) int {
	h := crc32.ChecksumIEEE([]byte(spriteName))
	return int(h%50000) + 10000
}

// SessionName returns the Mutagen session name for a sprite.
func SessionName(spriteName string) string {
	return "sprite-" + spriteName
}

// SSHHostAlias returns the SSH config host alias for Mutagen to use.
func SSHHostAlias(spriteName string) string {
	return "sprite-mutagen-" + spriteName
}

// SetupSSHServer installs and configures openssh-server on the sprite,
// adds the local user's public key to authorized_keys, and starts sshd.
func (m *Manager) SetupSSHServer(spriteName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	pubKeyPath := filepath.Join(home, ".ssh", "id_ed25519.pub")
	pubKey, err := os.ReadFile(pubKeyPath)
	if err != nil {
		return fmt.Errorf("reading SSH public key: %w", err)
	}

	// Install openssh-server if needed
	installCmd := `
		if ! command -v sshd >/dev/null 2>&1; then
			sudo apt-get update -qq && sudo apt-get install -y -qq openssh-server 2>/dev/null || true
		fi
	`
	if _, err := m.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", installCmd},
	}); err != nil {
		return fmt.Errorf("installing openssh-server: %w", err)
	}

	// Configure sshd for pubkey auth and add our key
	setupCmd := fmt.Sprintf(`
		mkdir -p ~/.ssh && chmod 700 ~/.ssh
		echo '%s' >> ~/.ssh/authorized_keys
		sort -u ~/.ssh/authorized_keys -o ~/.ssh/authorized_keys
		chmod 600 ~/.ssh/authorized_keys
		chown -R $(whoami) ~/.ssh
		sudo mkdir -p /run/sshd
		sudo sed -i 's/#PubkeyAuthentication yes/PubkeyAuthentication yes/' /etc/ssh/sshd_config 2>/dev/null || true
		sudo sed -i 's/PubkeyAuthentication no/PubkeyAuthentication yes/' /etc/ssh/sshd_config 2>/dev/null || true
	`, strings.TrimSpace(string(pubKey)))

	if _, err := m.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", setupCmd},
	}); err != nil {
		return fmt.Errorf("configuring SSH server: %w", err)
	}

	// Start sshd
	startCmd := `sudo systemctl start ssh 2>/dev/null || sudo service ssh start 2>/dev/null || sudo /usr/sbin/sshd 2>/dev/null`
	if _, err := m.client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", startCmd},
	}); err != nil {
		return fmt.Errorf("starting SSH server: %w", err)
	}

	return nil
}

// StartProxy starts a sprite proxy forwarding a local port to SSH port 22 on the sprite.
// Returns the proxy command (caller must manage the process) and the local port.
func (m *Manager) StartProxy(spriteName string) (*exec.Cmd, int, error) {
	port := ComputeSSHPort(spriteName)
	portMapping := fmt.Sprintf("%d:22", port)

	cmd, err := m.client.StartProxy(sprite.ProxyOptions{
		Sprite: spriteName,
		Ports:  []string{portMapping},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("starting proxy: %w", err)
	}

	// Wait for proxy to be listening
	if err := waitForPort(port, 30*time.Second); err != nil {
		cmd.Process.Kill()
		return nil, 0, fmt.Errorf("waiting for proxy port %d: %w", port, err)
	}

	return cmd, port, nil
}

// AddSSHConfig adds a temporary SSH config entry for Mutagen to use.
func AddSSHConfig(spriteName string, port int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	configPath := filepath.Join(home, ".ssh", "config")
	alias := SSHHostAlias(spriteName)

	entry := fmt.Sprintf(`
# sp-managed: %s
Host %s
  HostName localhost
  Port %d
  User sprite
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
# sp-end: %s
`, alias, alias, port, alias)

	// Remove old entry if present, then append new one
	if err := removeSSHConfigEntry(configPath, alias); err != nil {
		// Non-fatal: file might not exist yet
		_ = err
	}

	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening SSH config: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("writing SSH config entry: %w", err)
	}

	return nil
}

// RemoveSSHConfig removes the temporary SSH config entry for a sprite.
func RemoveSSHConfig(spriteName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}
	configPath := filepath.Join(home, ".ssh", "config")
	alias := SSHHostAlias(spriteName)
	return removeSSHConfigEntry(configPath, alias)
}

// removeSSHConfigEntry removes a managed SSH config block identified by the alias marker.
func removeSSHConfigEntry(configPath, alias string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	startMarker := "# sp-managed: " + alias
	endMarker := "# sp-end: " + alias

	lines := strings.Split(string(data), "\n")
	var result []string
	inBlock := false

	for _, line := range lines {
		if strings.TrimSpace(line) == startMarker {
			inBlock = true
			continue
		}
		if strings.TrimSpace(line) == endMarker {
			inBlock = false
			continue
		}
		if !inBlock {
			result = append(result, line)
		}
	}

	return os.WriteFile(configPath, []byte(strings.Join(result, "\n")), 0o600)
}

// StartMutagenSession creates a new Mutagen sync session between localDir and the sprite.
func (m *Manager) StartMutagenSession(spriteName, localDir, remoteDir string) (string, error) {
	sessionName := SessionName(spriteName)
	alias := SSHHostAlias(spriteName)

	// Build ignore arguments from .gitignore
	ignorePatterns := CollectIgnorePatterns(localDir)
	var ignoreArgs []string
	for _, p := range ignorePatterns {
		ignoreArgs = append(ignoreArgs, "--ignore", p)
	}

	// Build mutagen sync create command
	args := []string{"sync", "create",
		"--name", sessionName,
		"--sync-mode", "two-way-safe",
	}
	args = append(args, ignoreArgs...)
	args = append(args, localDir, fmt.Sprintf("%s:%s", alias, remoteDir))

	cmd := exec.Command("mutagen", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating mutagen session: %w\n%s", err, string(out))
	}

	// Get the session identifier
	id, err := getMutagenSessionID(sessionName)
	if err != nil {
		return sessionName, nil // Return name even if we can't get ID
	}

	return id, nil
}

// TerminateMutagenSession stops and removes a Mutagen sync session.
func TerminateMutagenSession(spriteName string) error {
	sessionName := SessionName(spriteName)
	cmd := exec.Command("mutagen", "sync", "terminate", sessionName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("terminating mutagen session %q: %w\n%s", sessionName, err, string(out))
	}
	return nil
}

// GetMutagenStatus queries the current status of a Mutagen sync session.
func GetMutagenStatus(spriteName string) (*SessionState, error) {
	sessionName := SessionName(spriteName)

	out, err := exec.Command("mutagen", "sync", "list", sessionName).Output()
	if err != nil {
		return nil, fmt.Errorf("listing mutagen session %q: %w", sessionName, err)
	}

	return parseMutagenStatus(sessionName, string(out)), nil
}

// parseMutagenStatus extracts session state from mutagen sync list output.
func parseMutagenStatus(name, output string) *SessionState {
	state := &SessionState{
		Name:   name,
		Status: "none",
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "Identifier:") {
			state.MutagenID = strings.TrimSpace(strings.TrimPrefix(line, "Identifier:"))
		}

		if strings.HasPrefix(line, "Status:") {
			statusText := strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
			state.Status = normalizeMutagenStatus(statusText)
		}

		if strings.HasPrefix(line, "Connected:") {
			connected := strings.TrimSpace(strings.TrimPrefix(line, "Connected:"))
			// This appears under Alpha and Beta sections
			if !state.AlphaConnected {
				state.AlphaConnected = connected == "Yes"
			} else {
				state.BetaConnected = connected == "Yes"
			}
		}

		if strings.Contains(line, "conflict") || strings.Contains(line, "Conflict") {
			// Try to extract conflict count
			parts := strings.Fields(line)
			for _, p := range parts {
				if n, err := strconv.Atoi(p); err == nil && n > 0 {
					state.Conflicts = n
					break
				}
			}
		}

		if strings.HasPrefix(line, "Last error:") {
			state.LastError = strings.TrimSpace(strings.TrimPrefix(line, "Last error:"))
		}
	}

	return state
}

// normalizeMutagenStatus converts Mutagen's verbose status to our short form.
func normalizeMutagenStatus(status string) string {
	status = strings.ToLower(status)
	switch {
	case strings.Contains(status, "watching"):
		return "watching"
	case strings.Contains(status, "scanning"):
		return "syncing"
	case strings.Contains(status, "staging"):
		return "syncing"
	case strings.Contains(status, "transitioning"):
		return "syncing"
	case strings.Contains(status, "saving"):
		return "syncing"
	case strings.Contains(status, "connecting"):
		return "connecting"
	case strings.Contains(status, "halted"):
		return "error"
	default:
		return "unknown"
	}
}

// getMutagenSessionID gets the session identifier from mutagen sync list.
func getMutagenSessionID(sessionName string) (string, error) {
	out, err := exec.Command("mutagen", "sync", "list", sessionName).Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Identifier:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Identifier:")), nil
		}
	}
	return "", fmt.Errorf("identifier not found in output")
}

// MutagenSessionExists checks if a named Mutagen session exists.
func MutagenSessionExists(spriteName string) bool {
	sessionName := SessionName(spriteName)
	err := exec.Command("mutagen", "sync", "list", sessionName).Run()
	return err == nil
}

// waitForPort polls until a local TCP port is accepting connections.
func waitForPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("lsof", "-i", fmt.Sprintf("tcp:%d", port), "-sTCP:LISTEN")
		if err := cmd.Run(); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("port %d not listening after %v", port, timeout)
}

// TestSSHConnection verifies SSH connectivity through the proxy.
func TestSSHConnection(spriteName string, port int) error {
	alias := SSHHostAlias(spriteName)

	// Clear any stale known_hosts entry
	exec.Command("ssh-keygen", "-R", fmt.Sprintf("[localhost]:%d", port)).Run()

	for attempt := 0; attempt < 10; attempt++ {
		cmd := exec.Command("ssh", "-o", "ConnectTimeout=5",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			alias, "echo", "ok")
		if out, err := cmd.CombinedOutput(); err == nil {
			if strings.Contains(string(out), "ok") {
				return nil
			}
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}

	return fmt.Errorf("SSH connection to %s (port %d) failed after 10 attempts", alias, port)
}
