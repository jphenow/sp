#!/usr/bin/env bash

set -euo pipefail

# sp - Sprite environment manager for GitHub repositories and local directories
# Usage:
#   sp owner/repo [--no-sync] [--name NAME] [-- COMMAND...]
#   sp . [--no-sync] [--name NAME] [-- COMMAND...]
#   sp info owner/repo|.
#   sp sessions owner/repo|.
#   sp conf {init|edit|show}
#
# Sprite naming:
#   GitHub repos:     gh-owner--repo      (e.g., gh-acme--widgets)
#   Local directories: local-<dirname>    (e.g., local-my-project)
#
# Options:
#   --no-sync     Disable bidirectional file syncing (sync is on by default)
#   --name NAME   tmux session name (default: derived from command name)
#   -- COMMAND    Command to run in the sprite (default: bash)
#
# Subcommands:
#   info       Show sprite info (name, existence, target dir, command, session) without connecting
#   sessions   List active tmux sessions in a sprite
#   conf       Manage setup config (~/.config/sprite/setup.conf)

# Configuration
CLAUDE_CONFIG_DIR="${HOME}/.claude"
SSH_KEY="${HOME}/.ssh/id_ed25519"
SSH_PUB_KEY="${HOME}/.ssh/id_ed25519.pub"
SPRITE_SETUP_CONF="${HOME}/.config/sprite/setup.conf"
SSH_CONFIG_CHANGED=""

# Sync configuration
SYNC_ENABLED=true
PROXY_PID=""
SYNC_SESSION_NAME=""
SYNC_SPRITE_NAME=""
SYNC_BG_PID=""
SSH_PORT=""

# Command and session configuration
EXEC_CMD="bash"
SESSION_NAME=""
RESOLVED_SPRITE_NAME=""
RESOLVED_TARGET_DIR=""
RESOLVED_REPO=""

# Batch check results (set by batch_check_sprite)
BATCH_AUTH=""
BATCH_DIR=""
BATCH_SSHD_READY=""

# Print usage information and exit
usage() {
    cat <<'EOF'
sp - Sprite environment manager for GitHub repositories and local directories

Usage:
  sp owner/repo [--no-sync] [--name NAME] [-- COMMAND...]
  sp . [--no-sync] [--name NAME] [-- COMMAND...]
  sp info owner/repo|. [--name NAME]
  sp sessions owner/repo|.
  sp conf {init|edit|show}

Subcommands:
  info       Show sprite metadata (name, existence, target dir, command, session)
             without connecting
  sessions   List active tmux sessions in a sprite
  conf       Manage setup config (~/.config/sprite/setup.conf)

Options:
  --no-sync     Disable bidirectional file syncing (sync is on by default)
  --sync        Explicitly enable sync (default, for clarity)
  --name NAME   tmux session name (default: derived from command name)
  --cmd CMD     Command to run (alternative to --)
  --help, -h    Show this help message

Commands:
  Everything after -- is the command to run in the sprite (default: bash).
  Use this for commands with flags: sp . -- claude -f foo bar baz

Sprite naming (in priority order):
  1. .sprite file   Read from local .sprite JSON file if present
  2. GitHub remote   gh-owner--repo      (e.g., gh-acme--widgets)
  3. Directory name  local-<dirname>     (e.g., local-my-project)

Setup config (~/.config/sprite/setup.conf):
  [files]
  ~/.local/bin/claude              # Copied to /home/sprite/.local/bin/claude
  ~/.config/opencode/config.toml   # ~ expands to $HOME locally, /home/sprite remotely

  [commands]
  # condition :: command (command runs when condition fails on sprite)
  ! command -v opencode :: curl -fsSL https://opencode.ai/install | bash &

Examples:
  sp superfly/flyctl                    # Work on a GitHub repo (opens bash)
  sp .                                  # Work on current directory
  sp . -- claude                        # Open claude in the sprite
  sp . -- claude -f foo bar baz         # Command with flags
  sp . --no-sync -- bash                 # Skip file sync
  sp . --name debug -- bash             # Named tmux session
  sp info .                             # Show sprite info without connecting
  sp sessions owner/repo                # List tmux sessions in a sprite
  sp conf init                          # Create setup config
  sp conf edit                          # Edit setup config
EOF
    exit 0
}

# Check for required commands
check_requirements() {
    local missing=()

    command -v sprite >/dev/null 2>&1 || missing+=("sprite")
    command -v git >/dev/null 2>&1 || missing+=("git")

    if [[ ${#missing[@]} -gt 0 ]]; then
        error "Missing required commands: ${missing[*]}"
    fi
}

# Check if Mutagen is installed
check_mutagen() {
    if ! command -v mutagen >/dev/null 2>&1; then
        error "Mutagen is not installed. Please install it with: brew install mutagen-io/mutagen/mutagen"
    fi
}

# Setup SSH server in sprite
# Installs OpenSSH server, configures it for key-based auth, and adds local public key.
# Short-circuits if sshd is already running and our public key is present.
setup_ssh_server() {
    local sprite_name="$1"

    # Fast path: if sshd is running and our key is already authorized, skip setup entirely.
    # This avoids ~6 sprite exec round trips on every reconnect.
    if [[ -f "$SSH_PUB_KEY" ]]; then
        local pubkey_content
        pubkey_content=$(cat "$SSH_PUB_KEY")
        if sprite exec -s "$sprite_name" sh -c "pgrep -x sshd >/dev/null 2>&1 && grep -qF '$pubkey_content' ~/.ssh/authorized_keys 2>/dev/null"; then
            info "SSH server already configured"
            return 0
        fi
    fi

    # Full setup path — flag so ensure_ssh_server_running knows to restart
    SSH_CONFIG_CHANGED=true
    info "Setting up SSH server in sprite..."

    # Check if SSH server is already installed.
    # Use test -x on the standard path since /usr/sbin may not be in PATH.
    if sprite exec -s "$sprite_name" test -x /usr/sbin/sshd 2>/dev/null; then
        info "SSH server already installed"
    else
        info "Installing OpenSSH server..."
        local install_attempts=0
        local max_install_attempts=3
        while [[ $install_attempts -lt $max_install_attempts ]]; do
            if sprite exec -s "$sprite_name" sh -c '
                export DEBIAN_FRONTEND=noninteractive
                sudo apt-get update -qq >/dev/null 2>&1
                sudo apt-get install -y -qq openssh-server >/dev/null 2>&1
            '; then
                break
            fi
            install_attempts=$((install_attempts + 1))
            if [[ $install_attempts -ge $max_install_attempts ]]; then
                warn "Failed to install SSH server after $max_install_attempts attempts"
                return 1
            fi
            warn "SSH server install attempt $install_attempts failed, retrying..."
            sleep 2
        done
    fi

    # Configure SSH for security
    info "Configuring SSH server..."
    sprite exec -s "$sprite_name" sh -c '
        sudo tee /etc/ssh/sshd_config.d/sprite.conf >/dev/null <<EOF
PubkeyAuthentication yes
PasswordAuthentication no
PermitRootLogin no
EOF
    ' || warn "Could not configure SSH server"

    # Ensure SSH host keys are generated
    info "Ensuring SSH host keys exist..."
    sprite exec -s "$sprite_name" sh -c 'sudo ssh-keygen -A 2>/dev/null' || true

    # Fix home directory ownership (SSH StrictModes requires this)
    info "Fixing directory permissions..."
    sprite exec -s "$sprite_name" sh -c 'sudo chown sprite:sprite /home/sprite 2>/dev/null' || true

    # Ensure .ssh directory exists with correct permissions and ownership
    sprite exec -s "$sprite_name" sh -c 'mkdir -p ~/.ssh && chmod 700 ~/.ssh && sudo chown -R sprite:sprite ~/.ssh 2>/dev/null' || true

    # Add local public key to authorized_keys if not already present
    if [[ -f "$SSH_PUB_KEY" ]]; then
        info "Adding public key to authorized_keys..."
        # Read the public key content
        local pubkey_content
        pubkey_content=$(cat "$SSH_PUB_KEY")

        # Add to authorized_keys using a safer method
        sprite exec -s "$sprite_name" sh -c "
            touch ~/.ssh/authorized_keys
            chmod 600 ~/.ssh/authorized_keys
            if ! grep -qF '$pubkey_content' ~/.ssh/authorized_keys 2>/dev/null; then
                echo '$pubkey_content' >> ~/.ssh/authorized_keys
            fi
        " || warn "Could not add public key to authorized_keys"
    fi

    info "SSH server setup complete"
}

# Ensure SSH server is running in sprite
# Ensures sshd is running. If already running, returns immediately.
# Only restarts if setup_ssh_server wrote new config (indicated by $SSH_CONFIG_CHANGED).
ensure_ssh_server_running() {
    local sprite_name="$1"

    # If sshd is already running and no config was changed, nothing to do
    if sprite exec -s "$sprite_name" pgrep -x sshd >/dev/null 2>&1; then
        if [[ "${SSH_CONFIG_CHANGED:-}" != "true" ]]; then
            info "SSH server already running"
            return 0
        fi
        # Config was changed by setup_ssh_server — restart to pick up changes
        info "Restarting SSH server to pick up config changes..."
        sprite exec -s "$sprite_name" sh -c 'sudo systemctl restart ssh 2>/dev/null || sudo service ssh restart 2>/dev/null || sudo killall sshd 2>/dev/null' || true
        sleep 2
    fi

    info "Starting SSH server..."

    # Try to start SSH service using systemd/init.d first (preferred method)
    if sprite exec -s "$sprite_name" sh -c 'sudo systemctl start ssh 2>/dev/null || sudo service ssh start 2>/dev/null'; then
        info "SSH service started via system service manager"
        sleep 2
    else
        # Fallback: start sshd manually in background
        info "Starting sshd manually..."
        # Use sprite exec with a backgrounded command that will persist
        sprite exec -s "$sprite_name" sh -c 'nohup sudo /usr/sbin/sshd -D >/tmp/sshd.log 2>&1 </dev/null & echo $! > /tmp/sshd.pid' 2>/dev/null || {
            warn "Failed to start sshd, checking configuration..."
            sprite exec -s "$sprite_name" sudo /usr/sbin/sshd -t 2>&1 || true
        }
        sleep 3
    fi

    # Verify sshd is running
    if ! sprite exec -s "$sprite_name" pgrep -x sshd >/dev/null 2>&1; then
        # Try to get diagnostic info
        warn "SSH server not detected, gathering diagnostics..."
        info "Checking SSH configuration:"
        sprite exec -s "$sprite_name" sudo /usr/sbin/sshd -t 2>&1 || true
        info "Checking SSH service status:"
        sprite exec -s "$sprite_name" sh -c 'sudo systemctl status ssh 2>&1 || sudo service ssh status 2>&1 || cat /tmp/sshd.log 2>&1' || true
        warn "SSH server did not start successfully"
        return 1
    fi

    info "SSH server started successfully"
}

# Return the lock directory path for a sprite's sync session.
# The lock dir holds: port (proxy port), proxy.pid, and per-user PID files.
sync_lock_dir() {
    echo "/tmp/sprite-sync-${1}.d"
}

# Register this sp process as a sync user for reference counting.
# Multiple sp processes can share one proxy + mutagen session.
register_sync_user() {
    local sprite_name="$1"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")
    mkdir -p "$lock_dir"
    echo "$$" > "${lock_dir}/$$.user"
}

# Unregister this process and check if any other sp processes remain.
# Returns 0 if we're the last user (caller should tear down sync).
# Returns 1 if others are still alive (leave sync running).
unregister_sync_user() {
    local sprite_name="$1"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")
    rm -f "${lock_dir}/$$.user" 2>/dev/null

    for pid_file in "${lock_dir}"/*.user; do
        [[ -f "$pid_file" ]] || continue
        local pid
        pid=$(cat "$pid_file" 2>/dev/null || echo "")
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            return 1  # Another user is still alive
        fi
        # Stale PID file — clean it up
        rm -f "$pid_file" 2>/dev/null
    done

    return 0  # We're the last user
}

# Check if sync infrastructure (proxy + mutagen) is already running for a sprite.
# If healthy, sets SSH_PORT from the existing proxy and returns 0.
has_active_sync() {
    local sprite_name="$1"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")
    local port_file="${lock_dir}/port"
    local proxy_pid_file="${lock_dir}/proxy.pid"

    # Need both a port file and a live proxy
    [[ -f "$port_file" ]] || return 1
    [[ -f "$proxy_pid_file" ]] || return 1

    local port proxy_pid
    port=$(cat "$port_file" 2>/dev/null || echo "")
    proxy_pid=$(cat "$proxy_pid_file" 2>/dev/null || echo "")

    [[ -n "$port" ]] || return 1
    [[ -n "$proxy_pid" ]] || return 1

    # Proxy process must be alive
    if ! kill -0 "$proxy_pid" 2>/dev/null; then
        return 1
    fi

    # Port must be listening
    if ! lsof -i ":${port}" >/dev/null 2>&1; then
        return 1
    fi

    # Mutagen session should exist (any status is OK — it may be scanning)
    local sync_name="sprite-${sprite_name}"
    if ! mutagen sync list 2>/dev/null | grep -q "Name: ${sync_name}$"; then
        return 1
    fi

    SSH_PORT="$port"
    return 0
}

# Path to the local state file tracking whether a sprite's one-time setup is done.
# Lives in /tmp so it resets on reboot.
sprite_state_file() {
    echo "/tmp/sprite-ready-${1}"
}

# Check if a sprite was previously set up by sp.
# Returns 1 (not ready) if setup.conf has been modified since last setup.
is_sprite_ready() {
    local state_file
    state_file=$(sprite_state_file "$1")
    [[ -f "$state_file" ]] || return 1

    # Re-run setup if the user has edited setup.conf since last run
    if [[ -f "$SPRITE_SETUP_CONF" ]] && [[ "$SPRITE_SETUP_CONF" -nt "$state_file" ]]; then
        return 1
    fi

    return 0
}

# Mark a sprite as fully set up after first-time configuration.
mark_sprite_ready() {
    touch "$(sprite_state_file "$1")"
}

# Check sprite state in a single remote call instead of 3+ sequential ones.
# Gathers auth, target directory existence, and SSH readiness in one roundtrip.
# Sets globals: BATCH_AUTH, BATCH_DIR, BATCH_SSHD_READY (each "y" or "n").
batch_check_sprite() {
    local sprite_name="$1"
    local target_dir="$2"

    local check_script=""
    check_script+="echo auth=\$(test -f ~/.ssh/id_ed25519 && echo y || echo n);"
    check_script+=" echo dir=\$(test -d '${target_dir}' && echo y || echo n);"

    if [[ -f "$SSH_PUB_KEY" ]]; then
        local pubkey_content
        pubkey_content=$(cat "$SSH_PUB_KEY")
        check_script+=" echo sshd=\$(pgrep -x sshd >/dev/null 2>&1 && grep -qF '${pubkey_content}' ~/.ssh/authorized_keys 2>/dev/null && echo y || echo n)"
    else
        check_script+=" echo sshd=n"
    fi

    local output
    if ! output=$(sprite exec -s "$sprite_name" sh -c "$check_script" 2>/dev/null); then
        BATCH_AUTH="n"
        BATCH_DIR="n"
        BATCH_SSHD_READY="n"
        return 1
    fi

    BATCH_AUTH="n"
    BATCH_DIR="n"
    BATCH_SSHD_READY="n"

    while IFS= read -r result_line; do
        case "$result_line" in
            auth=*) BATCH_AUTH="${result_line#auth=}" ;;
            dir=*) BATCH_DIR="${result_line#dir=}" ;;
            sshd=*) BATCH_SSHD_READY="${result_line#sshd=}" ;;
        esac
    done <<< "$output"
}

# Start sprite proxy for SSH port forwarding.
# Resolves port collisions by scanning nearby ports when the deterministic port
# is occupied. The proxy is disowned so it survives this process exiting —
# cleanup is handled explicitly by the last sync user.
start_sprite_proxy() {
    local sprite_name="$1"
    local base_port="$SSH_PORT"
    local port="$base_port"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")

    # Find an available port, starting from the deterministic base.
    # Tries up to 20 consecutive ports to handle hash collisions.
    local max_offset=20
    local offset=0
    while lsof -i ":${port}" >/dev/null 2>&1; do
        offset=$((offset + 1))
        if [[ $offset -ge $max_offset ]]; then
            warn "Could not find an available port in range ${base_port}-$((base_port + max_offset - 1)) for sprite '${sprite_name}'."
            return 1
        fi
        port=$((base_port + offset))
    done

    if [[ "$port" -ne "$base_port" ]]; then
        warn "Deterministic port ${base_port} was in use, using ${port} instead"
    fi
    SSH_PORT="$port"

    info "Starting port forwarding (localhost:${SSH_PORT} -> sprite:22)..."

    # Create a temporary log file for proxy output
    local proxy_log="/tmp/sprite-proxy-$$.log"

    # Start proxy in background and disown so it survives shell exit.
    # The last sp session to exit will explicitly kill it via the proxy.pid file.
    sprite proxy -s "$sprite_name" "${SSH_PORT}:22" >"$proxy_log" 2>&1 &
    PROXY_PID=$!
    disown "$PROXY_PID" 2>/dev/null || true

    # Wait for proxy to be ready
    local attempts=0
    local max_attempts=30
    while [[ $attempts -lt $max_attempts ]]; do
        # Check if the process is still running
        if ! kill -0 "$PROXY_PID" 2>/dev/null; then
            # Process died, show the log
            warn "Proxy process died unexpectedly. Log:"
            cat "$proxy_log" >&2
            rm -f "$proxy_log"
            warn "Port forwarding failed to start"
            return 1
        fi

        # Check if port is actually listening using lsof
        if lsof -i ":${SSH_PORT}" >/dev/null 2>&1; then
            # Write proxy metadata into the lock directory
            mkdir -p "$lock_dir"
            echo "$SSH_PORT" > "${lock_dir}/port"
            echo "$PROXY_PID" > "${lock_dir}/proxy.pid"
            info "Port forwarding ready"
            rm -f "$proxy_log"
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 0.5
    done

    # If we get here, show the log
    warn "Proxy did not become ready. Log:"
    cat "$proxy_log" >&2
    rm -f "$proxy_log"
    warn "Port forwarding failed to start"
    return 1
}

# Test SSH connectivity through the proxy
test_ssh_connection() {
    info "Testing SSH connection..."

    # First check if proxy is still running
    if ! kill -0 "$PROXY_PID" 2>/dev/null; then
        echo "ERROR: Proxy process (PID $PROXY_PID) died before SSH test could begin" >&2
        return 1
    fi

    # Check if port is actually listening
    if ! lsof -i ":${SSH_PORT}" >/dev/null 2>&1; then
        echo "ERROR: Port ${SSH_PORT} is not listening" >&2
        echo "Proxy status:" >&2
        ps -p "$PROXY_PID" >&2 || echo "Proxy process not found" >&2
        return 1
    fi

    # Remove any stale host key for localhost:SSH_PORT from known_hosts.
    # Each sprite has a different host key, and they all share this port via proxy,
    # so old entries will always cause "REMOTE HOST IDENTIFICATION HAS CHANGED" errors.
    ssh-keygen -R "[localhost]:${SSH_PORT}" 2>/dev/null || true

    local attempts=0
    local max_attempts=10
    while [[ $attempts -lt $max_attempts ]]; do
        # Try with verbose output to see what's failing
        # StrictHostKeyChecking=no is safe here — this is a local proxy to a sprite, not a real remote host
        local ssh_output
        ssh_output=$(ssh -p "$SSH_PORT" -o ConnectTimeout=2 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes sprite@localhost echo "ready" 2>&1)
        local ssh_exit=$?

        if [[ $ssh_exit -eq 0 ]]; then
            info "SSH connection successful"
            return 0
        fi

        # Show error on first and every 3rd attempt
        if [[ $attempts -eq 0 ]] || [[ $((attempts % 3)) -eq 0 ]]; then
            echo "WARNING: SSH connection attempt $((attempts + 1)) failed: $(echo "$ssh_output" | tail -1)" >&2
        fi

        attempts=$((attempts + 1))
        sleep 1
    done

    echo "ERROR: SSH connection test failed after $max_attempts attempts" >&2
    echo "Last error: $ssh_output" >&2
    return 1
}

# Start Mutagen sync session
# Creates bidirectional sync between local and remote directory
start_mutagen_sync() {
    local sprite_name="$1"
    local local_dir="$2"
    local remote_dir="$3"

    SYNC_SESSION_NAME="sprite-${sprite_name}"
    local ssh_config_marker="# mutagen-sprite-temp-${sprite_name}"

    info "Initializing Mutagen sync..."
    info "Local: $local_dir"
    info "Remote: sprite@localhost:$remote_dir"

    # Terminate any existing sessions with this name (stale or otherwise)
    if mutagen sync list 2>/dev/null | grep -q "Name: ${SYNC_SESSION_NAME}$"; then
        warn "Sync session already exists, terminating old session(s)..."
        mutagen sync terminate "$SYNC_SESSION_NAME" 2>/dev/null || true
        sleep 1
    fi

    # Add temporary SSH config entry to user's ~/.ssh/config
    info "Configuring SSH for Mutagen..."
    mkdir -p ~/.ssh
    touch ~/.ssh/config

    # Remove any existing entry for this sprite
    sed -i.bak "/${ssh_config_marker}/,+7d" ~/.ssh/config 2>/dev/null || true

    # Add new entry
    cat >> ~/.ssh/config <<EOF

${ssh_config_marker}
Host sprite-mutagen-${sprite_name}
    HostName localhost
    Port ${SSH_PORT}
    User sprite
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    IdentityFile ~/.ssh/id_ed25519
EOF

    # Create sync session
    mutagen sync create \
        --name "$SYNC_SESSION_NAME" \
        --sync-mode two-way-resolved \
        --ignore "node_modules" \
        --ignore ".next" \
        --ignore "dist" \
        --ignore "build" \
        --ignore ".DS_Store" \
        --ignore "._*" \
        "$local_dir" \
        "sprite-mutagen-${sprite_name}:${remote_dir}" || {
        warn "Failed to create Mutagen sync session"
        return 1
    }

    info "Waiting for initial sync to complete..."

    # Monitor sync status until initial sync completes
    local attempts=0
    local max_attempts=120  # 2 minutes max
    while [[ $attempts -lt $max_attempts ]]; do
        # Check sync status - look for "Status: Watching for changes" which indicates sync is ready
        local status
        status=$(mutagen sync list "$SYNC_SESSION_NAME" 2>/dev/null || echo "")

        # Check if sync is watching for changes (ready state)
        if echo "$status" | grep -q "Status: Watching for changes"; then
            info "Initial sync complete - watching for changes"
            return 0
        fi

        # Check for errors - only match "Status:" lines containing error,
        # not "Last error:" which is historical and may be from a stale session
        if echo "$status" | grep -q "Status:.*[Ee]rror"; then
            warn "Sync encountered an error: $status"
            return 1
        fi

        # Show progress every 5 seconds
        if [[ $((attempts % 10)) -eq 0 ]] && [[ $attempts -gt 0 ]]; then
            info "Syncing... (${attempts}s)"
        fi

        attempts=$((attempts + 1))
        sleep 0.5
    done

    warn "Initial sync did not complete in expected time, but continuing..."
}

# Stop Mutagen sync session and clean up SSH config
stop_mutagen_sync() {
    if [[ -n "$SYNC_SESSION_NAME" ]]; then
        info "Stopping Mutagen sync..."
        mutagen sync terminate "$SYNC_SESSION_NAME" 2>/dev/null || true

        # Clean up SSH config entry
        local ssh_config_marker="# mutagen-sprite-temp-"
        if [[ -f ~/.ssh/config ]]; then
            sed -i.bak "/${ssh_config_marker}/,+7d" ~/.ssh/config 2>/dev/null || true
        fi

        SYNC_SESSION_NAME=""
    fi
}

# Stop sprite proxy using the PID from the lock directory.
# Cleans up the lock directory afterwards.
stop_sprite_proxy() {
    local sprite_name="${1:-$SYNC_SPRITE_NAME}"
    [[ -n "$sprite_name" ]] || return 0

    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")
    local proxy_pid_file="${lock_dir}/proxy.pid"

    if [[ -f "$proxy_pid_file" ]]; then
        local proxy_pid
        proxy_pid=$(cat "$proxy_pid_file" 2>/dev/null || echo "")
        if [[ -n "$proxy_pid" ]]; then
            info "Stopping port forwarding..."
            kill "$proxy_pid" 2>/dev/null || true
        fi
    fi

    # Clean up the entire lock directory
    rm -rf "$lock_dir" 2>/dev/null || true
    PROXY_PID=""
    SYNC_SPRITE_NAME=""
}

# Run the full sync setup pipeline in the background.
# Called from a subshell — writes state to the lock directory so the main
# process's cleanup trap can tear things down on exit.
setup_sync_session() {
    local sprite_name="$1"
    local local_dir="$2"
    local target_dir="$3"

    local sync_ok=true

    # Skip SSH setup if batch check confirmed sshd is ready with our key
    if [[ "${BATCH_SSHD_READY:-}" == "y" ]]; then
        info "SSH server already configured"
    else
        setup_ssh_server "$sprite_name" || sync_ok=false

        if [[ "$sync_ok" == "true" ]]; then
            ensure_ssh_server_running "$sprite_name" || sync_ok=false
        fi
    fi

    if [[ "$sync_ok" == "true" ]]; then
        start_sprite_proxy "$sprite_name" || sync_ok=false
    fi

    if [[ "$sync_ok" == "true" ]] && ! test_ssh_connection; then
        sync_ok=false
    fi

    if [[ "$sync_ok" == "true" ]]; then
        start_mutagen_sync "$sprite_name" "$local_dir" "$target_dir" || sync_ok=false
    fi

    if [[ "$sync_ok" == "true" ]]; then
        info "✓ Sync is active — changes will be bidirectional"
    else
        warn "Bidirectional sync failed to start. File changes will not sync automatically."
    fi
}

# Cleanup function called on exit.
# Uses reference counting: only tears down sync infra when this is the last
# sp process using it. Otherwise leaves proxy + mutagen running for others.
cleanup_sync_session() {
    [[ -n "$SYNC_SPRITE_NAME" ]] || return 0

    echo ""  # Newline for cleaner output

    # Kill background sync process if still running
    if [[ -n "$SYNC_BG_PID" ]] && kill -0 "$SYNC_BG_PID" 2>/dev/null; then
        kill "$SYNC_BG_PID" 2>/dev/null || true
        wait "$SYNC_BG_PID" 2>/dev/null || true
    fi

    if unregister_sync_user "$SYNC_SPRITE_NAME"; then
        # We're the last user — tear everything down
        info "Cleaning up sync session..."
        stop_mutagen_sync
        stop_sprite_proxy "$SYNC_SPRITE_NAME"
    else
        info "Other sp sessions still active, leaving sync running"
    fi
}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Print error message and exit
error() {
    echo -e "${RED}Error: $1${NC}" >&2
    # Ensure the message is flushed before exit
    sleep 0.1
    exit 1
}

# Print info message
info() {
    echo -e "${GREEN}$1${NC}"
}

# Print warning message
warn() {
    echo -e "${YELLOW}$1${NC}"
}

# Check if sprite exists
sprite_exists() {
    local sprite_name="$1"
    sprite list 2>/dev/null | grep -q "^${sprite_name}$"
}

# Wait for sprite to be ready after creation
wait_for_sprite() {
    local sprite_name="$1"
    local max_attempts=60
    local attempt=0

    info "Waiting for sprite to be ready..."

    # First wait for basic connectivity
    while [[ $attempt -lt $max_attempts ]]; do
        if sprite exec -s "$sprite_name" echo "ready" 2>&1 >/dev/null; then
            # Extra sleep to ensure all services are up
            sleep 2
            info "Sprite is ready"
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 1

        # Show progress every 5 seconds
        if [[ $((attempt % 5)) -eq 0 ]]; then
            info "Still waiting... (${attempt}s)"
        fi
    done

    error "Sprite did not become ready in time"
}


# Get GitHub remote URL and extract owner/repo
# Returns empty string if not a git repo or no GitHub remote found
get_github_repo() {
    local remote_url
    remote_url=$(git config --get remote.origin.url 2>/dev/null) || return 0

    # Handle both SSH and HTTPS URLs
    # SSH: git@github.com:owner/repo.git
    # HTTPS: https://github.com/owner/repo.git
    if [[ "$remote_url" =~ github\.com[:/]([^/]+)/([^/]+)(\.git)?$ ]]; then
        local owner="${BASH_REMATCH[1]}"
        local repo="${BASH_REMATCH[2]}"
        repo="${repo%.git}"  # Remove .git suffix if present
        echo "${owner}/${repo}"
    fi
    # Returns empty if remote URL doesn't match GitHub pattern
}

# Convert owner/repo to sprite name
repo_to_sprite_name() {
    local repo="$1"
    echo "gh-${repo//\//--}"
}

# Convert directory name to sprite name (for non-GitHub directories)
dir_to_sprite_name() {
    local dir_name="$1"
    # Sanitize: lowercase, replace spaces/special chars with dashes
    local sanitized
    sanitized=$(echo "$dir_name" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g' | sed 's/--*/-/g' | sed 's/^-//;s/-$//')
    echo "local-${sanitized}"
}

# Replace characters that are invalid or problematic in tmux session names
# (spaces, periods, colons) with dashes
sanitize_session_name() {
    local name="$1"
    echo "$name" | sed 's/[.: ]/-/g'
}

# Return the tmux session name to use.
# Uses SESSION_NAME if explicitly set, otherwise derives from the first word
# of EXEC_CMD (the command name, ignoring arguments).
derive_session_name() {
    if [[ -n "$SESSION_NAME" ]]; then
        sanitize_session_name "$SESSION_NAME"
    else
        local cmd_name="${EXEC_CMD%% *}"
        sanitize_session_name "$cmd_name"
    fi
}

# Compute sprite name, target dir, and repo from a target (. or owner/repo)
# without creating anything. Sets RESOLVED_SPRITE_NAME, RESOLVED_TARGET_DIR,
# and RESOLVED_REPO globals.
resolve_sprite_info() {
    local target="$1"

    if [[ "$target" == "." ]]; then
        local current_dir
        current_dir=$(pwd)
        local dir_name
        dir_name=$(basename "$current_dir")

        local repo
        repo=$(get_github_repo)

        # Prefer .sprite file over computed naming
        local sprite_from_file
        sprite_from_file=$(get_current_dir_sprite)

        if [[ -n "$sprite_from_file" ]]; then
            RESOLVED_SPRITE_NAME="$sprite_from_file"
            RESOLVED_REPO="${repo:-}"
            if [[ -n "$repo" ]]; then
                RESOLVED_TARGET_DIR="/home/sprite/$(basename "$repo")"
            else
                RESOLVED_TARGET_DIR="/home/sprite/$dir_name"
            fi
        elif [[ -n "$repo" ]]; then
            RESOLVED_REPO="$repo"
            RESOLVED_SPRITE_NAME=$(repo_to_sprite_name "$repo")
            RESOLVED_TARGET_DIR="/home/sprite/$(basename "$repo")"
        else
            RESOLVED_REPO=""
            RESOLVED_SPRITE_NAME=$(dir_to_sprite_name "$dir_name")
            RESOLVED_TARGET_DIR="/home/sprite/$dir_name"
        fi
    elif [[ "$target" == */* ]]; then
        RESOLVED_REPO="$target"
        RESOLVED_SPRITE_NAME=$(repo_to_sprite_name "$target")
        RESOLVED_TARGET_DIR="/home/sprite/$(basename "$target")"
    else
        error "Invalid target. Use 'owner/repo' format or '.' for current directory"
    fi
}

# Execute EXEC_CMD inside a tmux session within the sprite.
# Uses tmux new-session -A to attach to an existing session or create a new one.
# Wrapped in sh -c so sprite exec doesn't consume tmux's -s flag as its own.
# EXEC_CMD is left unquoted inside the sh -c string so multi-word commands
# (e.g. "claude -c") are properly word-split into command + arguments.
exec_in_sprite() {
    local sprite_name="$1"
    local work_dir="$2"
    local claude_token="$3"

    local session_name
    session_name=$(derive_session_name)

    local shell_cmd
    shell_cmd=$(printf "exec tmux new-session -A -s '%s' %s" "$session_name" "$EXEC_CMD")

    sprite exec -s "$sprite_name" -dir "$work_dir" -env "CLAUDE_CODE_OAUTH_TOKEN=${claude_token}" -tty \
        sh -c "$shell_cmd"
}

# Print sprite metadata without connecting or creating anything.
handle_info() {
    local target="$1"
    resolve_sprite_info "$target"

    local exists="no"
    if sprite_exists "$RESOLVED_SPRITE_NAME"; then
        exists="yes"
    fi

    local session_name
    session_name=$(derive_session_name)

    echo "Sprite:    $RESOLVED_SPRITE_NAME"
    echo "Exists:    $exists"
    if [[ -n "$RESOLVED_REPO" ]]; then
        echo "Repo:      $RESOLVED_REPO"
    fi
    echo "Target:    $RESOLVED_TARGET_DIR"
    echo "Command:   $EXEC_CMD"
    echo "Session:   $session_name"
}

# List active tmux sessions in a sprite.
handle_sessions() {
    local target="$1"
    resolve_sprite_info "$target"

    if ! sprite_exists "$RESOLVED_SPRITE_NAME"; then
        error "Sprite '$RESOLVED_SPRITE_NAME' does not exist"
    fi

    sprite exec -s "$RESOLVED_SPRITE_NAME" tmux list-sessions 2>/dev/null || echo "No active tmux sessions"
}

# Handle 'sp conf' subcommand — manage ~/.config/sprite/setup.conf
handle_conf() {
    local action="${1:-}"

    case "$action" in
        init)
            conf_init
            ;;
        edit)
            conf_edit
            ;;
        show)
            conf_show
            ;;
        *)
            cat <<'EOF'
Usage: sp conf {init|edit|show}

  init   Create setup.conf with a starter template
  edit   Open setup.conf in $EDITOR
  show   Print setup.conf contents
EOF
            exit 1
            ;;
    esac
}

# Create ~/.config/sprite/setup.conf with a starter template
conf_init() {
    local conf_dir
    conf_dir=$(dirname "$SPRITE_SETUP_CONF")
    mkdir -p "$conf_dir"

    if [[ -f "$SPRITE_SETUP_CONF" ]]; then
        warn "Config already exists: $SPRITE_SETUP_CONF"
        echo "Use 'sp conf edit' to modify it."
        return 0
    fi

    cat > "$SPRITE_SETUP_CONF" <<'TMPL'
# Sprite setup configuration
# Files and commands here run automatically when connecting to any sprite.

[files]
# Paths to copy to the sprite. ~ expands to $HOME locally, /home/sprite remotely.
# Files are only copied if they don't already exist on the sprite.
# ~/.local/bin/claude
# ~/.config/opencode/config.toml

[commands]
# condition :: command
# Command runs on the sprite when condition succeeds (exits 0).
# ! command -v opencode :: curl -fsSL https://opencode.ai/install | bash &
TMPL

    info "Created $SPRITE_SETUP_CONF"
    echo "Edit with: sp conf edit"
}

# Open setup.conf in the user's editor
conf_edit() {
    if [[ ! -f "$SPRITE_SETUP_CONF" ]]; then
        conf_init
    fi
    "${EDITOR:-vi}" "$SPRITE_SETUP_CONF"
}

# Print setup.conf contents
conf_show() {
    if [[ ! -f "$SPRITE_SETUP_CONF" ]]; then
        echo "No config found. Create one with: sp conf init"
        return 0
    fi
    cat "$SPRITE_SETUP_CONF"
}

# Derive a deterministic SSH port from the sprite name.
# Hashes into range 10000-60000 so each sprite gets its own port,
# allowing concurrent sync sessions without port conflicts.
sprite_ssh_port() {
    local sprite_name="$1"
    local hash
    hash=$(echo -n "$sprite_name" | cksum | cut -d' ' -f1)
    echo $(( (hash % 50001) + 10000 ))
}

# Get the sprite name from .sprite in the current directory, if set.
# Prints the sprite value or nothing; used to avoid redundant 'sprite use'.
get_current_dir_sprite() {
    if [[ -f .sprite ]]; then
        tr -d '\n' < .sprite 2>/dev/null | sed -n 's/.*"sprite"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
    fi
}

# Get or create Claude OAuth token
get_claude_token() {
    # Check if CLAUDE_CODE_OAUTH_TOKEN already exists (use :- to handle unset)
    if [[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
        echo "$CLAUDE_CODE_OAUTH_TOKEN"
        return 0
    fi

    # Try to get from existing file
    if [[ -f "$HOME/.claude-token" ]]; then
        cat "$HOME/.claude-token"
        return 0
    fi

    # Need to generate new token - prompt user to paste it
    warn "No Claude OAuth token found." >&2
    echo "" >&2
    info "Please follow these steps:" >&2
    echo "  1. Open a new terminal window" >&2
    echo "  2. Run: claude setup-token" >&2
    echo "  3. Copy the ENTIRE token (all lines if it wraps)" >&2
    echo "  4. Paste it below" >&2
    echo "  5. Press Enter on an empty line when done" >&2
    echo "" >&2
    info "Paste token (input will be hidden, press Enter twice when done):" >&2

    # Read token - handle multi-line paste by reading until empty line
    local token_input=""
    local line

    # Disable echo for password-like input
    stty -echo

    # Read lines until we get an empty line
    while IFS= read -r line; do
        # If empty line, we're done
        if [[ -z "$line" ]]; then
            break
        fi
        token_input="${token_input}${line}"
    done

    # Re-enable echo
    stty echo
    echo "" >&2  # Print newline after silent input

    if [[ -z "$token_input" ]]; then
        error "No token provided"
    fi

    # Clean up any whitespace (handles newlines from terminal wrapping)
    token=$(echo "$token_input" | tr -d '[:space:]')

    # Validate token format
    if [[ ! "$token" =~ ^sk-ant-oat01- ]]; then
        error "Invalid token format. Token should start with 'sk-ant-oat01-'"
    fi

    # Validate token length (should be ~108 chars)
    if [[ ${#token} -lt 100 ]]; then
        warn "Warning: Token seems short (${#token} chars). Expected ~108 chars." >&2
        warn "Token may be incomplete. Please try pasting again." >&2
        error "Token validation failed"
    fi

    # Save token for future use
    printf "%s" "$token" > "$HOME/.claude-token"
    chmod 600 "$HOME/.claude-token"

    info "Token saved successfully (${#token} chars)" >&2

    echo "$token"
}

# Setup sprite with authentication files
setup_sprite_auth() {
    local sprite_name="$1"

    info "Setting up authentication for sprite: $sprite_name"

    # Get Claude OAuth token
    local claude_token
    claude_token=$(get_claude_token)

    if [[ -z "$claude_token" ]]; then
        error "Could not obtain Claude OAuth token"
    fi

    # Add token to shell rc files
    info "Configuring Claude OAuth token..."
    # Use printf to safely write the token, pass as env var
    sprite exec -s "$sprite_name" -env "CLAUDE_CODE_OAUTH_TOKEN=${claude_token}" bash -c '
        echo "" >> ~/.bashrc
        echo "# Claude Code OAuth Token" >> ~/.bashrc
        printf "export CLAUDE_CODE_OAUTH_TOKEN=\"%s\"\n" "$CLAUDE_CODE_OAUTH_TOKEN" >> ~/.bashrc

        echo "" >> ~/.zshrc
        echo "# Claude Code OAuth Token" >> ~/.zshrc
        printf "export CLAUDE_CODE_OAUTH_TOKEN=\"%s\"\n" "$CLAUDE_CODE_OAUTH_TOKEN" >> ~/.zshrc

        echo "Token configured in shell RC files"
    ' || warn "Could not set up OAuth token in shell rc files"

    # Setup minimal .claude.json with sprite defaults
    info "Setting up Claude config..."
    sprite exec -s "$sprite_name" sh -c 'echo "{\"bypassPermissionsModeAccepted\": true, \"numStartups\": 0, \"hasCompletedOnboarding\": true}" > ~/.claude.json'

    # Check if SSH key exists
    if [[ ! -f "$SSH_KEY" ]]; then
        warn "SSH key not found at $SSH_KEY"
    else
        info "Copying SSH key..."
        # First create .ssh directory
        sprite exec -s "$sprite_name" mkdir -p /home/sprite/.ssh

        # Upload SSH keys using absolute paths (required by sprite exec -file)
        if [[ -f "$SSH_PUB_KEY" ]]; then
            sprite exec -s "$sprite_name" -file "${SSH_KEY}:/home/sprite/.ssh/id_ed25519" -file "${SSH_PUB_KEY}:/home/sprite/.ssh/id_ed25519.pub" true || {
                warn "Could not upload SSH keys"
                return
            }
        else
            sprite exec -s "$sprite_name" -file "${SSH_KEY}:/home/sprite/.ssh/id_ed25519" true || {
                warn "Could not upload SSH key"
                return
            }
        fi

        # Set permissions and create config
        sprite exec -s "$sprite_name" sh -c 'chmod 700 ~/.ssh && chmod 600 ~/.ssh/id_ed25519 && if [ -f ~/.ssh/id_ed25519.pub ]; then chmod 644 ~/.ssh/id_ed25519.pub; fi && printf "Host github.com\n  StrictHostKeyChecking accept-new\n  IdentityFile ~/.ssh/id_ed25519\n" > ~/.ssh/config && chmod 600 ~/.ssh/config' || \
            warn "Could not set SSH permissions"
    fi
}

# Clone or pull repository in sprite
sync_repo() {
    local sprite_name="$1"
    local repo="$2"
    local repo_url="git@github.com:${repo}.git"
    local repo_dir=$(basename "$repo")
    local abs_repo_dir="/home/sprite/$repo_dir"

    info "Syncing repository: $repo"

    # Check if repo already exists in sprite
    if sprite exec -s "$sprite_name" test -d "$abs_repo_dir/.git" 2>/dev/null; then
        info "Repository exists, pulling latest changes..."
        sprite exec -s "$sprite_name" -dir "$abs_repo_dir" git pull --recurse-submodules
    else
        info "Cloning repository..."
        sprite exec -s "$sprite_name" git clone --recurse-submodules "$repo_url"
    fi
}

# Main function for owner/repo mode
handle_repo_mode() {
    local repo="$1"
    local sprite_name
    sprite_name=$(repo_to_sprite_name "$repo")

    info "Managing sprite for repository: $repo"
    info "Sprite name: $sprite_name"

    # Create sprite if it doesn't exist
    local is_new=false
    if ! sprite_exists "$sprite_name"; then
        info "Creating new sprite: $sprite_name"
        local create_output
        if create_output=$(sprite create -skip-console "$sprite_name" 2>&1); then
            wait_for_sprite "$sprite_name"
            is_new=true
        elif echo "$create_output" | grep -qi "already exists"; then
            info "Sprite already exists: $sprite_name"
        else
            error "Failed to create sprite: $create_output"
        fi
    else
        info "Sprite already exists: $sprite_name"
    fi

    # Setup: fast path skips auth/config/setup if sprite was previously configured
    if [[ "$is_new" != "true" ]] && is_sprite_ready "$sprite_name"; then
        info "Sprite previously configured, skipping setup"
    elif [[ "$is_new" == "true" ]]; then
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
        run_sprite_setup "$sprite_name"
        mark_sprite_ready "$sprite_name"
    else
        # Existing sprite, first connect: batch check + selective setup
        info "Checking sprite state..."
        batch_check_sprite "$sprite_name" "/home/sprite/$(basename "$repo")"

        if [[ "$BATCH_AUTH" != "y" ]]; then
            setup_sprite_auth "$sprite_name"
            setup_git_config "$sprite_name"
        fi

        run_sprite_setup "$sprite_name"
        mark_sprite_ready "$sprite_name"
    fi

    # Sync repository (always pull latest)
    sync_repo "$sprite_name" "$repo"

    # Open session in the repo directory
    local repo_dir=$(basename "$repo")
    local session_name
    session_name=$(derive_session_name)
    info "Opening ${EXEC_CMD} session in $repo_dir (tmux session: $session_name)..."

    local claude_token
    claude_token=$(get_claude_token)

    exec_in_sprite "$sprite_name" "/home/sprite/$repo_dir" "$claude_token"
}

# Ensure git config is set in sprite
setup_git_config() {
    local sprite_name="$1"

    # Get local git config
    local git_name git_email
    git_name=$(git config --get user.name 2>/dev/null || echo "")
    git_email=$(git config --get user.email 2>/dev/null || echo "")

    if [[ -n "$git_name" && -n "$git_email" ]]; then
        info "Configuring git..."
        sprite exec -s "$sprite_name" sh -c "git config --global user.name '$git_name' && git config --global user.email '$git_email'" 2>/dev/null || true
    fi
}

# Copy a single file to the sprite, expanding ~ for local and remote paths.
# Skips if the file already exists on the sprite.
# Preserves executable permission.
copy_setup_file() {
    local sprite_name="$1"
    local file_path="$2"

    # Trim whitespace
    file_path=$(echo "$file_path" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')

    # Expand ~ for local and remote paths
    local local_path="${file_path/#\~/$HOME}"
    local remote_path="${file_path/#\~//home/sprite}"

    if [[ ! -f "$local_path" ]]; then
        warn "Setup file not found locally: $local_path"
        return 0
    fi

    # Skip if file already exists on sprite
    if sprite exec -s "$sprite_name" test -f "$remote_path" 2>/dev/null; then
        return 0
    fi

    # Ensure destination directory exists
    local remote_dir
    remote_dir=$(dirname "$remote_path")
    sprite exec -s "$sprite_name" mkdir -p "$remote_dir" 2>/dev/null || true

    info "Copying $file_path to sprite..."
    if [[ -x "$local_path" ]]; then
        sprite exec -s "$sprite_name" -file "${local_path}:${remote_path}" \
            chmod +x "$remote_path" || warn "Failed to copy $file_path"
    else
        sprite exec -s "$sprite_name" -file "${local_path}:${remote_path}" \
            true || warn "Failed to copy $file_path"
    fi
}

# Run a conditional setup command on the sprite.
# Format: "condition :: command"
# The command runs when the condition succeeds (exits 0) on the sprite.
run_setup_command() {
    local sprite_name="$1"
    local line="$2"

    # Split on " :: " delimiter
    local condition="${line%% :: *}"
    local command="${line#* :: }"

    if [[ "$condition" == "$line" ]] || [[ -z "$condition" ]] || [[ -z "$command" ]]; then
        warn "Invalid setup command format (use 'condition :: command'): $line"
        return 0
    fi

    # Check condition on sprite — run command when condition succeeds (exits 0)
    if sprite exec -s "$sprite_name" sh -c "$condition" >/dev/null 2>&1; then
        info "Running: $command"
        # Run in background with keepalive dots so sprite exec doesn't
        # drop the connection during long-running installs.
        sprite exec -s "$sprite_name" sh -c "
            ($command) </dev/null &
            child=\$!
            while kill -0 \$child 2>/dev/null; do
                sleep 3
                printf '.'
            done
            echo ''
            wait \$child
        " 2>&1 || {
            warn "Setup command failed: $command"
        }
    fi
}

# Run sprite setup from ~/.config/sprite/setup.conf.
# Copies files and runs conditional commands defined in the config.
#
# Config format:
#   [files]
#   ~/.local/bin/claude
#   ~/.config/something/config.toml
#
#   [commands]
#   # condition :: command (command runs when condition succeeds on sprite)
#   ! command -v opencode :: curl -fsSL https://opencode.ai/install | bash &
run_sprite_setup() {
    local sprite_name="$1"

    if [[ ! -f "$SPRITE_SETUP_CONF" ]]; then
        return 0
    fi

    local section=""
    local has_work=false

    # Read on fd 3 so sprite exec calls inside the loop don't consume
    # the config file via inherited stdin.
    while IFS= read -r line <&3 || [[ -n "$line" ]]; do
        # Skip empty lines and comments
        [[ -z "$line" || "$line" =~ ^[[:space:]]*# ]] && continue

        # Detect section headers
        if [[ "$line" =~ ^\[([a-z]+)\]$ ]]; then
            section="${BASH_REMATCH[1]}"
            continue
        fi

        case "$section" in
            files)
                has_work=true
                copy_setup_file "$sprite_name" "$line"
                ;;
            commands)
                has_work=true
                run_setup_command "$sprite_name" "$line"
                ;;
            *)
                warn "Unknown section in setup.conf: [$section]"
                ;;
        esac
    done 3< "$SPRITE_SETUP_CONF"
}

# Sync current directory to sprite
# Args: sprite_name, target_dir_name (basename for /home/sprite/<name>), source_dir
sync_local_dir() {
    local sprite_name="$1"
    local target_dir_name="$2"
    local current_dir="$3"
    local target_dir="/home/sprite/$target_dir_name"

    info "Syncing local directory to sprite..."

    # Create a temporary tar file excluding git, node_modules, etc.
    # COPYFILE_DISABLE prevents macOS from creating ._* resource fork files
    local temp_tar="/tmp/sprite-sync-$$.tar.gz"
    COPYFILE_DISABLE=1 tar -czf "$temp_tar" \
        --no-xattrs \
        --exclude='node_modules' \
        --exclude='.next' \
        --exclude='dist' \
        --exclude='build' \
        --exclude='.DS_Store' \
        --exclude='._*' \
        -C "$(dirname "$current_dir")" \
        "$(basename "$current_dir")" 2>/dev/null || error "Failed to create archive"

    # Upload and extract in sprite (use absolute path for -file)
    info "Uploading directory contents..."
    sprite exec -s "$sprite_name" -file "${temp_tar}:/home/sprite/sync.tar.gz" \
        sh -c "cd ~ && tar -xzf sync.tar.gz 2>/dev/null && rm sync.tar.gz" || error "Failed to upload and extract"

    # Clean up
    rm -f "$temp_tar"

    info "Directory synced to $target_dir"
}

# Main function for current directory mode
handle_current_dir_mode() {
    local current_dir
    current_dir=$(pwd)
    local dir_name
    dir_name=$(basename "$current_dir")

    # Try to detect GitHub repo, but don't require it
    local repo
    repo=$(get_github_repo)

    local sprite_name
    local target_dir_name

    # Prefer .sprite file over computed naming
    local sprite_from_file
    sprite_from_file=$(get_current_dir_sprite)

    if [[ -n "$sprite_from_file" ]]; then
        sprite_name="$sprite_from_file"
        if [[ -n "$repo" ]]; then
            target_dir_name=$(basename "$repo")
        else
            target_dir_name="$dir_name"
        fi
        info "Using sprite from .sprite file: $sprite_name"
    elif [[ -n "$repo" ]]; then
        info "Detected GitHub repository: $repo"
        sprite_name=$(repo_to_sprite_name "$repo")
        target_dir_name=$(basename "$repo")
    else
        info "No GitHub repository detected, using directory name: $dir_name"
        sprite_name=$(dir_to_sprite_name "$dir_name")
        target_dir_name="$dir_name"
    fi

    info "Working directory: $current_dir"
    info "Sprite name: $sprite_name"

    # Create sprite if it doesn't exist
    local is_new=false
    if ! sprite_exists "$sprite_name"; then
        info "Creating new sprite: $sprite_name"
        local create_output
        if create_output=$(sprite create -skip-console "$sprite_name" 2>&1); then
            wait_for_sprite "$sprite_name"
            is_new=true
        elif echo "$create_output" | grep -qi "already exists"; then
            info "Sprite already exists: $sprite_name"
        else
            error "Failed to create sprite: $create_output"
        fi
    else
        info "Using existing sprite: $sprite_name"
    fi

    # Set this directory's default sprite so 'sprite' commands without -s use it
    local current_dir_sprite
    current_dir_sprite=$(get_current_dir_sprite)
    if [[ "$current_dir_sprite" != "$sprite_name" ]]; then
        info "Setting default sprite for this directory: $sprite_name"
        sprite use "$sprite_name"
    fi

    local target_dir="/home/sprite/$target_dir_name"

    # Set sync metadata early (needed for fast path check and sync setup)
    if [[ "$SYNC_ENABLED" == "true" ]]; then
        SYNC_SPRITE_NAME="$sprite_name"
        SSH_PORT=$(sprite_ssh_port "$sprite_name")

        # FAST PATH: sync already active from another sp session.
        # If sync infra (proxy + mutagen) is running, the sprite is fully set up.
        # Skip all setup and connect immediately.
        if has_active_sync "$sprite_name"; then
            info "Sync active, joining existing session (port ${SSH_PORT})"
            register_sync_user "$sprite_name"
            local claude_token
            claude_token=$(get_claude_token)
            exec_in_sprite "$sprite_name" "$target_dir" "$claude_token"
            return
        fi
    fi

    # --- Setup: three paths based on sprite state ---
    if [[ "$is_new" != "true" ]] && is_sprite_ready "$sprite_name"; then
        # FAST PATH: sprite previously set up (state file exists).
        # Skip auth, git config, sprite_setup, and dir check entirely.
        info "Sprite previously configured, skipping setup"
    elif [[ "$is_new" == "true" ]]; then
        # NEW SPRITE: full setup required
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
        run_sprite_setup "$sprite_name"
        sync_local_dir "$sprite_name" "$target_dir_name" "$current_dir"
        mark_sprite_ready "$sprite_name"
    else
        # EXISTING SPRITE, FIRST CONNECT: batch check + selective setup.
        # One remote call replaces 3-5 sequential sprite exec checks.
        info "Checking sprite state..."
        batch_check_sprite "$sprite_name" "$target_dir"

        if [[ "$BATCH_AUTH" != "y" ]]; then
            setup_sprite_auth "$sprite_name"
            setup_git_config "$sprite_name"
        fi

        run_sprite_setup "$sprite_name"

        if [[ "$BATCH_DIR" != "y" ]]; then
            sync_local_dir "$sprite_name" "$target_dir_name" "$current_dir"
        elif [[ "$SYNC_ENABLED" == "true" ]]; then
            info "Directory exists on sprite, skipping one-way sync (mutagen will handle it)"
        fi

        mark_sprite_ready "$sprite_name"
    fi

    # Setup bidirectional sync in background if enabled.
    # The sync pipeline (SSH, proxy, mutagen) runs asynchronously so the user
    # gets a shell immediately instead of waiting 5-15s for sync readiness.
    if [[ "$SYNC_ENABLED" == "true" ]]; then
        # SYNC_SPRITE_NAME and SSH_PORT already set above.
        # Pre-set SYNC_SESSION_NAME so cleanup can find the mutagen session.
        SYNC_SESSION_NAME="sprite-${sprite_name}"
        register_sync_user "$sprite_name"

        local sync_log="/tmp/sprite-sync-${sprite_name}.log"
        info "Starting sync in background (log: $sync_log)"

        (
            setup_sync_session "$sprite_name" "$current_dir" "$target_dir"
        ) > "$sync_log" 2>&1 &
        SYNC_BG_PID=$!
    fi

    # Open session — the user gets a shell immediately
    local session_name
    session_name=$(derive_session_name)
    info "Opening ${EXEC_CMD} session in $target_dir_name (tmux session: $session_name)..."

    local claude_token
    claude_token=$(get_claude_token)

    exec_in_sprite "$sprite_name" "$target_dir" "$claude_token"
}

# Set up cleanup trap
trap cleanup_sync_session EXIT INT TERM

# Main entry point
main() {
    check_requirements

    if [[ $# -eq 0 ]]; then
        usage
    fi

    local subcommand=""
    local target=""

    # Check for help flag or subcommands
    case "$1" in
        --help|-h|help)
            usage
            ;;
        conf)
            shift
            handle_conf "$@"
            exit 0
            ;;
        info|sessions)
            subcommand="$1"
            shift
            if [[ $# -eq 0 ]]; then
                error "Usage: sp $subcommand owner/repo|."
            fi
            target="$1"
            shift
            ;;
        *)
            target="$1"
            shift
            ;;
    esac

    # Parse flags. Everything after -- becomes the command to run.
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --help|-h)
                usage
                ;;
            --sync)
                SYNC_ENABLED=true
                shift
                ;;
            --no-sync)
                SYNC_ENABLED=false
                shift
                ;;
            --cmd)
                if [[ $# -lt 2 ]]; then
                    error "--cmd requires an argument"
                fi
                EXEC_CMD="$2"
                shift 2
                ;;
            --name)
                if [[ $# -lt 2 ]]; then
                    error "--name requires an argument"
                fi
                SESSION_NAME="$2"
                shift 2
                ;;
            --)
                shift
                if [[ $# -gt 0 ]]; then
                    EXEC_CMD="$*"
                fi
                break
                ;;
            *)
                error "Unknown flag: $1"
                ;;
        esac
    done

    # Check Mutagen if sync is enabled
    if [[ "$SYNC_ENABLED" == "true" ]]; then
        check_mutagen
    fi

    # Dispatch subcommands
    case "$subcommand" in
        info)
            handle_info "$target"
            ;;
        sessions)
            handle_sessions "$target"
            ;;
        "")
            # No subcommand — launch mode
            case "$target" in
                .)
                    handle_current_dir_mode
                    ;;
                */*)
                    handle_repo_mode "$target"
                    ;;
                *)
                    error "Invalid argument. Use 'owner/repo' format or '.' for current directory"
                    ;;
            esac
            ;;
    esac
}

main "$@"
