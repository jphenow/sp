#!/usr/bin/env bash

set -euo pipefail

# sp - Sprite environment manager for GitHub repositories and local directories
# Usage:
#   sp owner/repo [--sync] [--cmd CMD] [--name NAME]
#   sp . [--sync] [--cmd CMD] [--name NAME]
#   sp info owner/repo [--cmd CMD] [--name NAME]
#   sp info .
#   sp sessions owner/repo
#   sp sessions .
#
# Sprite naming:
#   GitHub repos:     gh-owner--repo      (e.g., gh-acme--widgets)
#   Local directories: local-<dirname>    (e.g., local-my-project)
#
# Options:
#   --sync        Enable bidirectional file syncing with Mutagen (requires: brew install mutagen)
#   --cmd CMD     Command to run instead of 'claude' (default: claude)
#   --name NAME   tmux session name (default: derived from command name)
#
# Subcommands:
#   info       Show sprite info (name, existence, target dir, command, session) without connecting
#   sessions   List active tmux sessions in a sprite

# Configuration
CLAUDE_CONFIG_DIR="${HOME}/.claude"
SSH_KEY="${HOME}/.ssh/id_ed25519"
SSH_PUB_KEY="${HOME}/.ssh/id_ed25519.pub"

# Sync configuration
SYNC_ENABLED=false
PROXY_PID=""
SYNC_SESSION_NAME=""
SYNC_SPRITE_NAME=""
SSH_PORT=""

# Command and session configuration
EXEC_CMD="claude"
SESSION_NAME=""
RESOLVED_SPRITE_NAME=""
RESOLVED_TARGET_DIR=""
RESOLVED_REPO=""

# Print usage information and exit
usage() {
    cat <<'EOF'
sp - Sprite environment manager for GitHub repositories and local directories

Usage:
  sp owner/repo [--sync] [--cmd CMD] [--name NAME]
  sp . [--sync] [--cmd CMD] [--name NAME]
  sp info owner/repo|. [--cmd CMD] [--name NAME]
  sp sessions owner/repo|.

Subcommands:
  info       Show sprite metadata (name, existence, target dir, command, session)
             without connecting
  sessions   List active tmux sessions in a sprite

Options:
  --sync        Enable bidirectional file syncing with Mutagen
  --cmd CMD     Command to run instead of 'claude' (default: claude)
  --name NAME   tmux session name (default: derived from command name)
  --help, -h    Show this help message

Sprite naming (in priority order):
  1. .sprite file   Read from local .sprite JSON file if present
  2. GitHub remote   gh-owner--repo      (e.g., gh-acme--widgets)
  3. Directory name  local-<dirname>     (e.g., local-my-project)

Examples:
  sp superfly/flyctl                  # Work on a GitHub repo
  sp .                                # Work on current directory
  sp . --cmd bash --name debug        # Open bash in a named tmux session
  sp . --sync                         # Enable bidirectional file sync
  sp info .                           # Show sprite info without connecting
  sp sessions owner/repo              # List tmux sessions in a sprite
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
# Installs OpenSSH server, configures it for key-based auth, and adds local public key
setup_ssh_server() {
    local sprite_name="$1"

    info "Setting up SSH server in sprite..."

    # Check if SSH server is already installed
    if sprite exec -s "$sprite_name" command -v sshd >/dev/null 2>&1; then
        info "SSH server already installed"
    else
        info "Installing OpenSSH server..."
        sprite exec -s "$sprite_name" sh -c '
            export DEBIAN_FRONTEND=noninteractive
            sudo apt-get update -qq >/dev/null 2>&1
            sudo apt-get install -y -qq openssh-server >/dev/null 2>&1
        ' || error "Failed to install SSH server"
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
# Restarts sshd if already running to pick up config changes, starts it otherwise
ensure_ssh_server_running() {
    local sprite_name="$1"

    # Always restart sshd to ensure it picks up any config changes from setup_ssh_server.
    # A stale sshd process (from a previous session or auto-started by apt) may have
    # outdated config or host keys, causing "Connection reset by peer" errors.
    if sprite exec -s "$sprite_name" pgrep -x sshd >/dev/null 2>&1; then
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
        error "SSH server did not start successfully"
    fi

    info "SSH server started successfully"
}

# Start sprite proxy for SSH port forwarding
# Resolves port collisions by scanning nearby ports when the deterministic port
# is occupied. Writes a lock file so stale sessions can be detected.
start_sprite_proxy() {
    local sprite_name="$1"
    local base_port="$SSH_PORT"
    local port="$base_port"
    local lock_dir="/tmp"
    local lock_file="${lock_dir}/sprite-sync-${sprite_name}.port"

    # If a lock file exists from a previous run of this sprite, check if it's stale
    if [[ -f "$lock_file" ]]; then
        local old_port old_pid
        read -r old_port old_pid < "$lock_file" 2>/dev/null || true
        if [[ -n "$old_pid" ]] && kill -0 "$old_pid" 2>/dev/null; then
            error "Another sync session for sprite '${sprite_name}' is already running (PID ${old_pid}, port ${old_port})."
        fi
        # Previous owner is dead — stale lock. Clean up and continue.
        rm -f "$lock_file"
    fi

    # Find an available port, starting from the deterministic base.
    # Tries up to 20 consecutive ports to handle hash collisions.
    local max_offset=20
    local offset=0
    while lsof -i ":${port}" >/dev/null 2>&1; do
        offset=$((offset + 1))
        if [[ $offset -ge $max_offset ]]; then
            error "Could not find an available port in range ${base_port}-$((base_port + max_offset - 1)) for sprite '${sprite_name}'."
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

    # Start proxy in background with logging
    sprite proxy -s "$sprite_name" "${SSH_PORT}:22" >"$proxy_log" 2>&1 &
    PROXY_PID=$!

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
            error "Port forwarding failed to start"
        fi

        # Check if port is actually listening using lsof
        if lsof -i ":${SSH_PORT}" >/dev/null 2>&1; then
            # Write lock file: port and PID so other sessions can detect us
            echo "${SSH_PORT} $$" > "$lock_file"
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
    error "Port forwarding failed to start"
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

    # Check if session already exists
    if mutagen sync list 2>/dev/null | grep -q "^${SYNC_SESSION_NAME}$"; then
        warn "Sync session already exists, terminating old session..."
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
        --ignore-vcs \
        --ignore "node_modules" \
        --ignore ".next" \
        --ignore "dist" \
        --ignore "build" \
        --ignore ".DS_Store" \
        --ignore "._*" \
        "$local_dir" \
        "sprite-mutagen-${sprite_name}:${remote_dir}" || {
        error "Failed to create Mutagen sync session"
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

        # Check for errors
        if echo "$status" | grep -qi "error"; then
            error "Sync encountered an error: $status"
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

# Stop Mutagen sync session
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

# Stop sprite proxy and remove its lock file
stop_sprite_proxy() {
    if [[ -n "$PROXY_PID" ]]; then
        info "Stopping port forwarding..."
        kill "$PROXY_PID" 2>/dev/null || true
        PROXY_PID=""
    fi
    # Remove the port lock file so other sessions don't see us as alive
    if [[ -n "$SYNC_SPRITE_NAME" ]]; then
        rm -f "/tmp/sprite-sync-${SYNC_SPRITE_NAME}.port"
        SYNC_SPRITE_NAME=""
    fi
}

# Cleanup function called on exit
cleanup_sync_session() {
    if [[ "$SYNC_ENABLED" == "true" ]]; then
        echo ""  # Newline for cleaner output
        info "Cleaning up sync session..."
        stop_mutagen_sync
        stop_sprite_proxy
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
# Uses SESSION_NAME if explicitly set, otherwise derives from EXEC_CMD.
derive_session_name() {
    if [[ -n "$SESSION_NAME" ]]; then
        sanitize_session_name "$SESSION_NAME"
    else
        sanitize_session_name "$EXEC_CMD"
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
        sprite create -skip-console "$sprite_name"
        wait_for_sprite "$sprite_name"
        is_new=true
    else
        info "Sprite already exists: $sprite_name"
    fi

    # Check if SSH key exists in sprite, set up auth if missing
    if [[ "$is_new" == "true" ]] || ! sprite exec -s "$sprite_name" test -f /home/sprite/.ssh/id_ed25519 2>/dev/null; then
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
    fi

    # Sync repository
    sync_repo "$sprite_name" "$repo"

    # Open session in the repo directory
    local repo_dir=$(basename "$repo")
    local session_name
    session_name=$(derive_session_name)
    info "Opening ${EXEC_CMD} session in $repo_dir (tmux session: $session_name)..."

    # Get the token to pass as env var
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
        sprite create -skip-console "$sprite_name"
        wait_for_sprite "$sprite_name"
        is_new=true
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

    # Check if SSH key exists in sprite, set up auth if missing
    if [[ "$is_new" == "true" ]] || ! sprite exec -s "$sprite_name" test -f /home/sprite/.ssh/id_ed25519 2>/dev/null; then
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
    fi

    # Sync local directory to sprite (initial one-way sync)
    sync_local_dir "$sprite_name" "$target_dir_name" "$current_dir"

    # Setup bidirectional sync if enabled
    if [[ "$SYNC_ENABLED" == "true" ]]; then
        # Derive a per-sprite SSH port so concurrent sync sessions don't collide
        SSH_PORT=$(sprite_ssh_port "$sprite_name")
        SYNC_SPRITE_NAME="$sprite_name"
        info "Setting up bidirectional sync (port ${SSH_PORT})..."

        # Setup SSH server in sprite
        setup_ssh_server "$sprite_name"

        # Ensure SSH server is running
        ensure_ssh_server_running "$sprite_name"

        # Start sprite proxy for SSH port forwarding
        start_sprite_proxy "$sprite_name"

        # Test SSH connection
        if ! test_ssh_connection; then
            error "Failed to establish SSH connection to sprite. Cannot start Mutagen sync."
        fi

        # Start Mutagen sync
        start_mutagen_sync "$sprite_name" "$current_dir" "/home/sprite/$target_dir_name"

        info ""
        info "✓ Sync is active - changes will be bidirectional"
        info ""
    fi

    # Open session in the synced directory
    local session_name
    session_name=$(derive_session_name)
    info "Opening ${EXEC_CMD} session in $target_dir_name (tmux session: $session_name)..."

    # Get the token to pass as env var
    local claude_token
    claude_token=$(get_claude_token)

    exec_in_sprite "$sprite_name" "/home/sprite/$target_dir_name" "$claude_token"
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

    # Parse flags
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --help|-h)
                usage
                ;;
            --sync)
                SYNC_ENABLED=true
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
