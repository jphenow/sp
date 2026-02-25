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

# Output control
VERBOSE=false

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
  sp status owner/repo|.
  sp sessions owner/repo|.
  sp resync .
  sp conf {init|edit|show}

Subcommands:
  info       Show sprite metadata (name, existence, target dir, command, session)
             without connecting
  status     Show sync health, conflicts, proxy state, and active users
  sessions   List active tmux sessions in a sprite
  resync     Tear down and restart Mutagen sync (re-reads .gitignore rules)
  conf       Manage setup config (~/.config/sprite/setup.conf)

Options:
  --no-sync     Disable bidirectional file syncing (sync is on by default)
  --sync        Explicitly enable sync (default, for clarity)
  --name NAME   tmux session name (default: derived from command name)
  --cmd CMD     Command to run (alternative to --)
  --verbose, -v Show detailed progress output
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
  ~/.local/bin/claude              # Copied if local is newer (default)
  ~/.config/opencode/config.toml   # ~ expands to $HOME locally, /home/sprite remotely
  ~/.config/sprite/foo -> ~/.bar   # Different source and destination paths
  ~/.tmux.conf [always]            # Always overwrite, even if remote is newer

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
  sp status .                              # Check sync health and conflicts
  sp resync .                             # Reset sync (re-reads .gitignore)
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

# Install gum if missing. Non-fatal — script degrades to printing titles without animation.
ensure_gum() {
    command -v gum >/dev/null 2>&1 && return 0
    if [[ "$(uname)" == "Darwin" ]] && command -v brew >/dev/null 2>&1; then
        brew install gum >/dev/null 2>&1 && return 0
    fi
    if [[ "$(uname)" == "Linux" ]]; then
        local arch
        arch=$(uname -m)
        [[ "$arch" == "x86_64" ]] && arch="amd64"
        [[ "$arch" == "aarch64" ]] && arch="arm64"
        local url="https://github.com/charmbracelet/gum/releases/download/v0.16.2/gum_0.16.2_Linux_${arch}.tar.gz"
        mkdir -p "$HOME/.local/bin"
        curl -fsSL "$url" | tar -xz -C "$HOME/.local/bin" gum 2>/dev/null && {
            export PATH="$HOME/.local/bin:$PATH"
            return 0
        }
    fi
    return 1
}

# Run a function under a gum spinner. Falls back to printing the title if gum is unavailable.
# Uses self-invocation: gum spin runs "$0 _run func args..." so gum has a real process to monitor.
spin() {
    local title="$1"; shift
    if command -v gum >/dev/null 2>&1; then
        gum spin --show-error --spinner dot --title "$title" -- "$0" _run "$@"
    else
        printf "  %s\n" "$title"
        "$@"
    fi
}

# Print a user-facing status line. Always visible regardless of --verbose.
msg() {
    echo "$1"
}

# Setup SSH server in sprite
# Installs OpenSSH server, configures it for key-based auth, and adds local public key.
# Short-circuits if sshd is already running and our public key is present.
setup_ssh_server() {
    local sprite_name="$1"

    # Fast path: if sshd is running and our key is already authorized, skip
    # full setup. Always fix ownership regardless — sprite exec -file uploads
    # as ubuntu:ubuntu and can clobber /home/sprite and ~/.ssh ownership at
    # any time (e.g. copy_always_files on reconnect), which causes sshd
    # StrictModes to reject pubkey auth even though the key is present.
    if [[ -f "$SSH_PUB_KEY" ]]; then
        local pubkey_content
        pubkey_content=$(cat "$SSH_PUB_KEY")
        if sprite exec -s "$sprite_name" sh -c "
            # Fix ownership unconditionally (cheap, prevents StrictModes failures)
            chown sprite:sprite /home/sprite 2>/dev/null
            chown -R sprite:sprite ~/.ssh 2>/dev/null
            chmod 700 ~/.ssh 2>/dev/null
            # Then check if sshd is running and key is authorized
            pgrep -x sshd >/dev/null 2>&1 && grep -qF '$pubkey_content' ~/.ssh/authorized_keys 2>/dev/null
        "; then
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
# If the proxy is alive but mutagen is missing or errored, attempts recovery
# by recreating the mutagen session on the existing proxy — avoids orphaning
# the proxy by starting a duplicate on a different port.
# Args: sprite_name [local_dir] [remote_dir]
#   local_dir/remote_dir are needed for recovery; if omitted, recovery is skipped.
has_active_sync() {
    local sprite_name="$1"
    local local_dir="${2:-}"
    local remote_dir="${3:-}"
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

    SSH_PORT="$port"

    # Mutagen session should exist (any status is OK — it may be scanning)
    local sync_name="sprite-${sprite_name}"
    local sync_output
    sync_output=$(mutagen sync list "$sync_name" 2>/dev/null || echo "")

    if echo "$sync_output" | grep -q "Name: ${sync_name}$"; then
        # Session exists — check for error state
        if echo "$sync_output" | grep -q "Status:.*[Ee]rror"; then
            warn "Mutagen sync session in error state, resetting..."
            mutagen sync reset "$sync_name" 2>/dev/null || true
        fi
        return 0
    fi

    # Proxy is alive but mutagen session is gone — try to recover.
    # Without local/remote dirs we can't recreate the session.
    if [[ -z "$local_dir" ]] || [[ -z "$remote_dir" ]]; then
        return 1
    fi

    warn "Mutagen sync session missing, recreating on existing proxy (port $port)..."
    recover_mutagen_session "$sprite_name" "$port" "$local_dir" "$remote_dir"
    return $?
}

# Recreate a mutagen sync session using an existing healthy proxy.
# Called when the proxy is alive but the mutagen session disappeared
# (e.g., daemon restart, crash). Ensures the SSH config points at the
# right port, then creates a fresh session and waits for initial sync.
recover_mutagen_session() {
    local sprite_name="$1"
    local port="$2"
    local local_dir="$3"
    local remote_dir="$4"
    local sync_name="sprite-${sprite_name}"
    local ssh_config_marker="# mutagen-sprite-temp-${sprite_name}"

    # Ensure SSH config entry points at the existing proxy port
    mkdir -p ~/.ssh
    touch ~/.ssh/config
    sed -i.bak "/${ssh_config_marker}/,+7d" ~/.ssh/config 2>/dev/null || true

    cat >> ~/.ssh/config <<EOF

${ssh_config_marker}
Host sprite-mutagen-${sprite_name}
    HostName localhost
    Port ${port}
    User sprite
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    IdentityFile ~/.ssh/id_ed25519
EOF

    # Verify SSH actually works through the proxy before creating the
    # Mutagen session. The proxy process may be alive but the underlying
    # connection to the sprite could be stale (sprite restarted, sshd
    # stopped, etc.). Clear stale host keys first.
    ssh-keygen -R "[localhost]:${port}" 2>/dev/null || true

    local ssh_ok=false
    local ssh_attempt=0
    while [[ $ssh_attempt -lt 3 ]]; do
        if ssh -p "$port" -o ConnectTimeout=2 -o StrictHostKeyChecking=no \
               -o UserKnownHostsFile=/dev/null -o BatchMode=yes \
               sprite@localhost echo "ready" >/dev/null 2>&1; then
            ssh_ok=true
            break
        fi
        ssh_attempt=$((ssh_attempt + 1))
        sleep 1
    done

    if [[ "$ssh_ok" != "true" ]]; then
        warn "SSH connection through existing proxy failed — proxy is stale"
        return 1
    fi

    # Terminate any leftover session with this name (shouldn't exist, but be safe)
    mutagen sync terminate "$sync_name" 2>/dev/null || true

    # Build dynamic ignore flags from .gitignore (or hardcoded fallback)
    local -a ignore_args=()
    while IFS= read -r _flag; do
        ignore_args+=("$_flag")
    done < <(build_mutagen_ignore_args "$local_dir")

    # Create the sync session.
    # two-way-safe ensures conflicts are flagged rather than silently resolved
    # in favor of alpha (local). This prevents sprite-side changes from being
    # overwritten when a session is recreated and the merge baseline is lost.
    mutagen sync create \
        --name "$sync_name" \
        --sync-mode two-way-safe \
        "${ignore_args[@]}" \
        "$local_dir" \
        "sprite-mutagen-${sprite_name}:${remote_dir}" || {
        warn "Failed to recreate Mutagen sync session"
        return 1
    }

    # Wait briefly for initial sync
    local attempts=0
    local max_attempts=60  # 30 seconds
    while [[ $attempts -lt $max_attempts ]]; do
        local status
        status=$(mutagen sync list "$sync_name" 2>/dev/null || echo "")

        if echo "$status" | grep -q "Status: Watching for changes"; then
            info "Sync session recovered — watching for changes"
            return 0
        fi

        if echo "$status" | grep -q "Status:.*[Ee]rror"; then
            warn "Recovered sync session hit an error"
            return 1
        fi

        attempts=$((attempts + 1))
        sleep 0.5
    done

    warn "Recovered sync session did not complete initial sync in time, but continuing..."
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
# Kills any orphaned proxy processes for the same sprite before starting,
# so we always land on the deterministic port. The proxy is disowned so it
# survives this process exiting — cleanup is handled by the last sync user.
start_sprite_proxy() {
    local sprite_name="$1"
    local base_port="$SSH_PORT"
    local port="$base_port"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")

    # Kill any orphaned sprite proxy processes for this sprite.
    # These can survive when a previous sp session's cleanup races with the
    # disowned proxy (e.g., background sync subshell was killed before it
    # could record the PID, or the lock dir was already cleaned up).
    local stale_pid
    while IFS= read -r stale_pid; do
        [[ -n "$stale_pid" ]] || continue
        info "Killing orphaned proxy (PID $stale_pid) for $sprite_name"
        kill "$stale_pid" 2>/dev/null || true
    done < <(pgrep -f "sprite proxy -s ${sprite_name} " 2>/dev/null || true)

    # Brief pause to let the port(s) free up after killing orphans
    if lsof -i ":${port}" >/dev/null 2>&1; then
        sleep 0.5
    fi

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

# Collect ignore patterns from all .gitignore files in a git repository.
# Walks the repo to find .gitignore files, reads each one, strips comments
# and blank lines, and prefixes patterns from nested .gitignore files with
# their relative directory path so Mutagen applies them correctly.
# Outputs one pattern per line to stdout.
collect_gitignore_patterns() {
    local repo_dir="$1"

    # Only works for git repos
    if ! git -C "$repo_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        return 0
    fi

    # Find all .gitignore files in the repo, excluding dirs that are
    # themselves typically gitignored (avoid descending into huge trees).
    local gitignore_files
    gitignore_files=$(find "$repo_dir" \
        -name .git -prune -o \
        -name node_modules -prune -o \
        -name _build -prune -o \
        -name deps -prune -o \
        -name .elixir_ls -prune -o \
        -name .gitignore -print 2>/dev/null | sort)

    local gitignore_file
    while IFS= read -r gitignore_file; do
        [[ -z "$gitignore_file" ]] && continue

        # Compute the directory containing this .gitignore, relative to repo root.
        local gitignore_dir
        gitignore_dir=$(dirname "$gitignore_file")
        local rel_dir
        # Compute relative path without python dependency.
        # realpath --relative-to is available on macOS 13+ and GNU coreutils.
        if rel_dir=$(realpath --relative-to="$repo_dir" "$gitignore_dir" 2>/dev/null); then
            : # success
        elif [[ "$gitignore_dir" == "$repo_dir" ]]; then
            rel_dir="."
        else
            # Fallback: strip the repo_dir prefix
            rel_dir="${gitignore_dir#"$repo_dir"/}"
        fi

        # Read patterns from this .gitignore
        local line
        while IFS= read -r line || [[ -n "$line" ]]; do
            # Skip empty lines and comments
            [[ -z "$line" ]] && continue
            [[ "$line" =~ ^[[:space:]]*# ]] && continue
            # Trim leading/trailing whitespace
            line=$(echo "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
            [[ -z "$line" ]] && continue

            local is_negation=false
            local pattern="$line"

            # Handle negation patterns (! prefix)
            if [[ "$pattern" == !* ]]; then
                is_negation=true
                pattern="${pattern#!}"
            fi

            # Strip leading / (Mutagen paths are relative to sync root)
            pattern="${pattern#/}"
            # Strip trailing / (Mutagen matches dirs by name without trailing slash)
            pattern="${pattern%/}"

            # Skip empty patterns after stripping
            [[ -z "$pattern" ]] && continue

            # For nested .gitignore files, prefix pattern with relative dir
            if [[ "$rel_dir" != "." ]]; then
                pattern="${rel_dir}/${pattern}"
            fi

            # Reassemble with negation prefix if needed
            if [[ "$is_negation" == true ]]; then
                echo "!${pattern}"
            else
                echo "$pattern"
            fi
        done < "$gitignore_file"
    done <<< "$gitignore_files"
}

# Build an array of --ignore flags for mutagen sync create.
# For git repos, reads .gitignore patterns and merges them with a baseline
# set of hardcoded ignores. Always adds !.git to ensure the .git directory
# is synced (for branch state). For non-git dirs, uses only the hardcoded set.
# Outputs the flags space-separated to stdout, suitable for eval.
build_mutagen_ignore_args() {
    local local_dir="$1"

    # Baseline ignores that apply regardless of .gitignore
    local -a patterns=(
        "node_modules"
        ".next"
        "dist"
        "build"
        ".DS_Store"
        "._*"
    )

    # If it's a git repo, collect .gitignore patterns
    if git -C "$local_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        local gitignore_pattern
        while IFS= read -r gitignore_pattern; do
            [[ -z "$gitignore_pattern" ]] && continue
            # Avoid duplicates with baseline patterns
            local is_dup=false
            local existing
            for existing in "${patterns[@]}"; do
                if [[ "$existing" == "$gitignore_pattern" ]]; then
                    is_dup=true
                    break
                fi
            done
            if [[ "$is_dup" == false ]]; then
                patterns+=("$gitignore_pattern")
            fi
        done < <(collect_gitignore_patterns "$local_dir")

        # Ensure .git is never ignored — we want branch state to sync
        patterns+=("!.git")
    else
        # Non-git directories: add common build artifact patterns
        patterns+=(
            "_build"
            "deps"
            ".elixir_ls"
        )
    fi

    # Build the --ignore flag string
    local p
    for p in "${patterns[@]}"; do
        printf -- '--ignore\n%s\n' "$p"
    done
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

    # Build dynamic ignore flags from .gitignore (or hardcoded fallback)
    local -a ignore_args=()
    while IFS= read -r _flag; do
        ignore_args+=("$_flag")
    done < <(build_mutagen_ignore_args "$local_dir")

    # Create sync session.
    # two-way-safe ensures conflicts are flagged rather than silently resolved
    # in favor of alpha (local). This prevents sprite-side changes from being
    # overwritten when a session is recreated and the merge baseline is lost.
    mutagen sync create \
        --name "$SYNC_SESSION_NAME" \
        --sync-mode two-way-safe \
        "${ignore_args[@]}" \
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
        # Check for conflicts after sync stabilizes
        check_sync_conflicts "$sprite_name"
    else
        warn "Bidirectional sync failed to start. File changes will not sync automatically."
    fi
}

# Check for sync conflicts and warn the user.
# With two-way-safe mode, conflicts are flagged rather than auto-resolved.
# This function checks for them and prints a visible warning so the user
# knows to intervene.
check_sync_conflicts() {
    local sprite_name="$1"
    local sync_name="sprite-${sprite_name}"

    local sync_output
    sync_output=$(mutagen sync list "$sync_name" 2>/dev/null || echo "")

    # Short format shows "Conflicts: <N>" where N > 0.
    # Guard every pipeline with || true — set -euo pipefail will kill
    # the script if grep finds no matches (exit 1) in any pipeline.
    local conflict_count
    conflict_count=$(echo "$sync_output" | grep "^Conflicts:" | sed 's/^Conflicts: *//' | tr -d ' ' || true)

    # No conflicts line or zero — nothing to report
    [[ -n "$conflict_count" ]] || return 0
    [[ "$conflict_count" -gt 0 ]] 2>/dev/null || return 0

    # Get conflict details from long format
    local long_output
    long_output=$(mutagen sync list -l "$sync_name" 2>/dev/null || echo "")

    # Extract the (alpha)/(beta) conflict paths
    local conflict_paths
    conflict_paths=$(echo "$long_output" | grep -E '\(alpha\)|\(beta\)' | sed 's/^[[:space:]]*/  /' | head -20 || true)

    echo ""
    echo -e "\033[1;33m━━━ Sync conflicts ($conflict_count) ━━━\033[0m"
    if [[ -n "$conflict_paths" ]]; then
        echo "$conflict_paths"
        if [[ "$conflict_count" -gt 10 ]]; then
            echo "  ... and more"
        fi
    fi
    echo ""
    echo -e "\033[1;33mTo resolve:\033[0m"
    echo -e "\033[1;33m  mutagen sync list -l $sync_name   # see all conflicts\033[0m"
    echo -e "\033[1;33m  mutagen sync reset $sync_name     # accept current state as baseline\033[0m"
    echo ""
}

# Reset terminal mouse tracking modes.
# The remote tmux enables SGR mouse tracking (modes 1000/1003/1006).
# If the connection drops, the local terminal is still in mouse mode
# but the consumer is gone — causing raw escape sequences like
# "65;40;62M" to print on scroll/click. Disabling all mouse modes
# returns the pane to normal.
reset_terminal() {
    printf '\e[?1000l\e[?1003l\e[?1006l' 2>/dev/null || true
}

# Cleanup function called on exit.
# Uses reference counting: only tears down sync infra when this is the last
# sp process using it. Otherwise leaves proxy + mutagen running for others.
# When this is the last user, waits a grace period before tearing down to
# allow a reconnecting sp process to register — avoids destroying sync
# infrastructure during brief connection drops / session recovery.
# Always resets terminal mouse modes regardless of how we exited.
cleanup() {
    reset_terminal

    [[ -n "$SYNC_SPRITE_NAME" ]] || return 0

    # Kill background sync process if still running
    if [[ -n "$SYNC_BG_PID" ]] && kill -0 "$SYNC_BG_PID" 2>/dev/null; then
        kill "$SYNC_BG_PID" 2>/dev/null || true
        wait "$SYNC_BG_PID" 2>/dev/null || true
    fi

    if unregister_sync_user "$SYNC_SPRITE_NAME"; then
        # We're the last user — defer teardown with a grace period.
        # A reconnecting sp process can register during this window,
        # inheriting the running proxy + mutagen session intact.
        # Run the deferred teardown in a detached background process so
        # the user's terminal isn't blocked waiting for the grace period.
        local _sprite="$SYNC_SPRITE_NAME"
        local _session="$SYNC_SESSION_NAME"
        (
            local grace_seconds=30
            local elapsed=0
            local lock_dir
            lock_dir=$(sync_lock_dir "$_sprite")

            while [[ $elapsed -lt $grace_seconds ]]; do
                sleep 1
                elapsed=$((elapsed + 1))

                # Check if a new sp process registered as a sync user
                for pid_file in "${lock_dir}"/*.user; do
                    [[ -f "$pid_file" ]] || continue
                    local pid
                    pid=$(cat "$pid_file" 2>/dev/null || echo "")
                    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
                        # A new sp process showed up — leave sync running
                        exit 0
                    fi
                done
            done

            # Nobody reconnected — tear everything down
            if [[ -n "$_session" ]]; then
                mutagen sync terminate "$_session" 2>/dev/null || true
                local ssh_config_marker="# mutagen-sprite-temp-"
                if [[ -f ~/.ssh/config ]]; then
                    sed -i.bak "/${ssh_config_marker}/,+7d" ~/.ssh/config 2>/dev/null || true
                fi
            fi

            local proxy_pid_file="${lock_dir}/proxy.pid"
            if [[ -f "$proxy_pid_file" ]]; then
                local proxy_pid
                proxy_pid=$(cat "$proxy_pid_file" 2>/dev/null || echo "")
                if [[ -n "$proxy_pid" ]]; then
                    kill "$proxy_pid" 2>/dev/null || true
                fi
            fi
            rm -rf "$lock_dir" 2>/dev/null || true
        ) &
        disown $! 2>/dev/null || true
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

# Print info message. Quiet by default; only prints in verbose mode.
# Inside gum spin subprocesses, output is captured and shown on error.
info() {
    if [[ "$VERBOSE" == "true" ]]; then
        echo -e "${GREEN}$1${NC}"
    fi
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
# Uses mkdir -p to ensure the work directory exists (initial sync may have
# failed for large repos) and cd instead of -dir so the command still runs.
exec_in_sprite() {
    local sprite_name="$1"
    local work_dir="$2"
    local claude_token="$3"

    local session_name
    session_name=$(derive_session_name)

    local shell_cmd
    shell_cmd=$(printf "mkdir -p '%s' && cd '%s' && exec tmux new-session -A -s '%s' %s" \
        "$work_dir" "$work_dir" "$session_name" "$EXEC_CMD")

    sprite exec -s "$sprite_name" -env "CLAUDE_CODE_OAUTH_TOKEN=${claude_token}" -tty \
        sh -c "$shell_cmd" || true
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

# Handle 'sp status' subcommand — show sync health, conflicts, and session state.
handle_status() {
    local target="$1"
    resolve_sprite_info "$target"

    local sprite_name="$RESOLVED_SPRITE_NAME"

    echo "Sprite: $sprite_name"

    if ! sprite_exists "$sprite_name"; then
        echo "State:  does not exist"
        return
    fi
    echo "State:  exists"

    # Check sync infrastructure
    local sync_name="sprite-${sprite_name}"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")

    # Proxy status
    local proxy_pid_file="${lock_dir}/proxy.pid"
    if [[ -f "$proxy_pid_file" ]]; then
        local proxy_pid
        proxy_pid=$(cat "$proxy_pid_file" 2>/dev/null || echo "")
        if [[ -n "$proxy_pid" ]] && kill -0 "$proxy_pid" 2>/dev/null; then
            local port
            port=$(cat "${lock_dir}/port" 2>/dev/null || echo "?")
            echo "Proxy:  running (pid $proxy_pid, port $port)"
        else
            echo "Proxy:  dead (stale pid file)"
        fi
    else
        echo "Proxy:  not running"
    fi

    # Mutagen session status
    local sync_output
    sync_output=$(mutagen sync list "$sync_name" 2>/dev/null || echo "")

    if echo "$sync_output" | grep -q "Name: ${sync_name}$"; then
        local status_line
        status_line=$(echo "$sync_output" | grep "^Status:" | head -1 || true)
        echo "Sync:   ${status_line#Status: }"

        # Show mode from long format
        local long_output
        long_output=$(mutagen sync list -l "$sync_name" 2>/dev/null || echo "")
        local mode_line
        mode_line=$(echo "$long_output" | grep "Synchronization mode:" | head -1 || true)
        if [[ -n "$mode_line" ]]; then
            echo "Mode:   $(echo "$mode_line" | sed 's/.*: //')"
        fi

        # Show last error if any
        local last_error
        last_error=$(echo "$sync_output" | grep "^Last error:" | head -1 || true)
        if [[ -n "$last_error" ]]; then
            echo -e "\033[1;33m${last_error}\033[0m"
        fi

        # Check for conflicts
        check_sync_conflicts "$sprite_name"
    else
        echo "Sync:   no session"
    fi

    # Active users
    local user_count=0
    if [[ -d "$lock_dir" ]]; then
        for pid_file in "${lock_dir}"/*.user; do
            [[ -f "$pid_file" ]] || continue
            local pid
            pid=$(cat "$pid_file" 2>/dev/null || echo "")
            if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
                user_count=$((user_count + 1))
            fi
        done
    fi
    echo "Users:  $user_count active sp process(es)"
}

# Handle 'sp resync' subcommand — tear down and restart the Mutagen sync session.
# Terminates the existing proxy + Mutagen session, cleans up state, and
# relaunches the sync pipeline in a detached background process. Returns
# immediately so the caller's terminal is not blocked.
handle_resync() {
    local target="$1"
    resolve_sprite_info "$target"

    local sprite_name="$RESOLVED_SPRITE_NAME"
    local target_dir="$RESOLVED_TARGET_DIR"

    if ! sprite_exists "$sprite_name"; then
        error "Sprite '$sprite_name' does not exist"
    fi

    # Resolve the local directory for sync (needed for .gitignore reading
    # and as the Mutagen alpha endpoint).
    local local_dir
    if [[ "$target" == "." ]]; then
        local_dir=$(pwd)
    else
        error "resync only works with '.' (current directory mode)"
    fi

    local sync_name="sprite-${sprite_name}"
    local lock_dir
    lock_dir=$(sync_lock_dir "$sprite_name")

    # If an existing Mutagen session is running, flush pending changes first
    # so both sides are in agreement before we destroy the baseline.
    # This prevents the new session from treating sprite-side changes as
    # conflicts (which alpha/local would win, silently overwriting them).
    if mutagen sync list "$sync_name" 2>/dev/null | grep -q "Name: ${sync_name}$"; then
        echo "Flushing pending sync changes..."
        # Flush with a timeout — it can block indefinitely if the remote
        # is unreachable. Run in a background subshell and wait with a
        # deadline for portability (macOS lacks coreutils timeout).
        (mutagen sync flush "$sync_name" 2>/dev/null) &
        local flush_pid=$!
        local flush_ok=false
        local flush_waited=0
        while [[ $flush_waited -lt 15 ]]; do
            if ! kill -0 "$flush_pid" 2>/dev/null; then
                wait "$flush_pid" 2>/dev/null && flush_ok=true
                break
            fi
            sleep 1
            flush_waited=$((flush_waited + 1))
        done
        if [[ "$flush_ok" == "true" ]]; then
            echo "  Flush complete"
        else
            kill "$flush_pid" 2>/dev/null || true
            wait "$flush_pid" 2>/dev/null || true
            echo "  Flush timed out or failed (continuing anyway)"
        fi
    fi

    echo "Tearing down existing sync for $sprite_name..."

    # 1. Terminate Mutagen session
    if mutagen sync list "$sync_name" 2>/dev/null | grep -q "Name: ${sync_name}$"; then
        mutagen sync terminate "$sync_name" 2>/dev/null || true
        echo "  Mutagen session terminated"
    else
        echo "  No Mutagen session found"
    fi

    # 2. Kill the proxy if running
    local proxy_pid_file="${lock_dir}/proxy.pid"
    if [[ -f "$proxy_pid_file" ]]; then
        local proxy_pid
        proxy_pid=$(cat "$proxy_pid_file" 2>/dev/null || echo "")
        if [[ -n "$proxy_pid" ]] && kill -0 "$proxy_pid" 2>/dev/null; then
            kill "$proxy_pid" 2>/dev/null || true
            echo "  Proxy stopped (pid $proxy_pid)"
        fi
    fi

    # 3. Clean up SSH config entries for this sprite
    local ssh_config_marker="# mutagen-sprite-temp-${sprite_name}"
    if [[ -f ~/.ssh/config ]]; then
        sed -i.bak "/${ssh_config_marker}/,+7d" ~/.ssh/config 2>/dev/null || true
    fi

    # 4. Remove the lock directory (stale state)
    rm -rf "$lock_dir" 2>/dev/null || true
    echo "  Lock directory cleaned"

    # 5. Restart the sync pipeline in a fully detached subprocess.
    #    We avoid setting global SYNC_SPRITE_NAME / SYNC_SESSION_NAME so the
    #    cleanup trap doesn't interfere. The detached process registers itself
    #    as a sync user while it runs, and the next 'sp .' will inherit the
    #    infra via has_active_sync().
    echo "Starting fresh sync..."

    local sync_log="/tmp/sprite-sync-${sprite_name}.log"
    local ssh_port
    ssh_port=$(sprite_ssh_port "$sprite_name")

    (
        # Export state that setup_sync_session / start_mutagen_sync need
        SSH_PORT="$ssh_port"
        SYNC_SPRITE_NAME="$sprite_name"
        SYNC_SESSION_NAME="$sync_name"
        VERBOSE=true

        # Register so the infra isn't orphaned during startup.
        # A subsequent 'sp .' will see the live user file and join.
        register_sync_user "$sprite_name"

        setup_sync_session "$sprite_name" "$local_dir" "$target_dir"
        local rc=$?

        # If setup failed, unregister so stale state doesn't linger.
        # On success, leave the registration — the next 'sp .' will
        # take over, and cleanup's grace period handles the gap.
        if [[ $rc -ne 0 ]]; then
            unregister_sync_user "$sprite_name"
        fi
    ) > "$sync_log" 2>&1 &
    disown $! 2>/dev/null || true

    echo "Sync restarting in background (log: $sync_log)"
    echo "Run 'sp .' to reconnect and inherit the new session."
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
# Default: only copy if local file is newer than remote (or remote is missing).
# Append [always] to always overwrite, or [newest] to be explicit about default.
# Use "source -> dest" to copy from a different local path.
# ~/.local/bin/claude
# ~/.config/opencode/config.toml
# ~/.config/sprite/tmux.conf.local -> ~/.tmux.conf.local [always]

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

# Composite function for spin(): create sprite + wait for readiness.
# Called via spin() in a subprocess — all side effects are remote or file-based.
_do_create_sprite() {
    local sprite_name="$1"
    local create_output
    if create_output=$(sprite create -skip-console "$sprite_name" 2>&1); then
        wait_for_sprite "$sprite_name"
    elif echo "$create_output" | grep -qi "already exists"; then
        info "Sprite already exists: $sprite_name"
    else
        error "Failed to create sprite: $create_output"
    fi
}

# Composite function for spin(): full sprite setup (auth, git, config, optional sync + mark ready).
# Called via spin() in a subprocess. Reads env vars for options.
# Env: _SETUP_TARGET_DIR, _SETUP_TARGET_DIR_NAME, _SETUP_CURRENT_DIR,
#      _SETUP_IS_NEW, _SETUP_MODE (dir|repo), CLAUDE_CODE_OAUTH_TOKEN
_do_setup_sprite() {
    local sprite_name="$1"

    if [[ "${_SETUP_IS_NEW}" == "true" ]]; then
        # New sprite: full setup
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
        run_sprite_setup "$sprite_name"
        if [[ "${_SETUP_MODE}" == "dir" ]]; then
            sync_local_dir "$sprite_name" "$_SETUP_TARGET_DIR_NAME" "$_SETUP_CURRENT_DIR" || true
        fi
        mark_sprite_ready "$sprite_name"
    else
        # Existing sprite, first connect: batch check + selective setup
        batch_check_sprite "$sprite_name" "$_SETUP_TARGET_DIR"

        if [[ "$BATCH_AUTH" != "y" ]]; then
            setup_sprite_auth "$sprite_name"
            setup_git_config "$sprite_name"
        fi

        run_sprite_setup "$sprite_name"

        # Skip tar sync for existing sprites — their files may have been
        # updated remotely and a naive tar overwrite would clobber those
        # changes. Mutagen (if enabled) will reconcile diffs incrementally.

        mark_sprite_ready "$sprite_name"
    fi

    # Always (re)install the open wrapper so we can iterate on it
    install_open_wrapper "$sprite_name"
}

# Composite function for spin(): clone or pull repository.
_do_sync_repo() {
    local sprite_name="$1"
    local repo="$2"
    sync_repo "$sprite_name" "$repo"
}

# Composite function for spin(): all preflight work for current-dir mode.
# Covers existence check, sprite use, creation if needed, setup if needed.
# Called via spin() in a subprocess. All side effects are remote or file-based,
# so the parent can re-derive cheap state after the spinner completes.
# Env: _PREFLIGHT_TARGET_DIR, _PREFLIGHT_TARGET_DIR_NAME, _PREFLIGHT_CURRENT_DIR,
#      CLAUDE_CODE_OAUTH_TOKEN
_do_connect_preflight() {
    local sprite_name="$1"

    # Create sprite if it doesn't exist
    if ! sprite_exists "$sprite_name"; then
        _do_create_sprite "$sprite_name"
        # Full setup for new sprite
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
        run_sprite_setup "$sprite_name"
        sync_local_dir "$sprite_name" "$_PREFLIGHT_TARGET_DIR_NAME" "$_PREFLIGHT_CURRENT_DIR" || true
        mark_sprite_ready "$sprite_name"
        return
    fi

    # Set default sprite for this directory
    sprite use "$sprite_name" >/dev/null 2>&1 || true

    # Setup if not previously configured
    if ! is_sprite_ready "$sprite_name"; then
        batch_check_sprite "$sprite_name" "$_PREFLIGHT_TARGET_DIR"

        if [[ "$BATCH_AUTH" != "y" ]]; then
            setup_sprite_auth "$sprite_name"
            setup_git_config "$sprite_name"
        fi

        run_sprite_setup "$sprite_name"

        # Skip tar sync for existing sprites — their files may have been
        # updated remotely and a naive tar overwrite would clobber those
        # changes. Mutagen (if enabled) will reconcile diffs incrementally.

        mark_sprite_ready "$sprite_name"
    else
        # Sprite is already set up — still refresh [always] files
        # so things like auth tokens stay current between connects.
        copy_always_files "$sprite_name"
    fi

    # Always (re)install the open wrapper so we can iterate on it
    install_open_wrapper "$sprite_name"
}

# Main function for owner/repo mode
handle_repo_mode() {
    local repo="$1"
    local sprite_name
    sprite_name=$(repo_to_sprite_name "$repo")

    # Pre-acquire token before spinners (interactive prompt can't run inside gum spin)
    local claude_token
    claude_token=$(get_claude_token)
    export CLAUDE_CODE_OAUTH_TOKEN="$claude_token"

    # Create sprite if it doesn't exist
    local is_new=false
    if ! sprite_exists "$sprite_name"; then
        spin "Creating sprite ${sprite_name}..." _do_create_sprite "$sprite_name"
        is_new=true
    fi

    # Setup: fast path skips auth/config/setup if sprite was previously configured
    if [[ "$is_new" != "true" ]] && is_sprite_ready "$sprite_name"; then
        # Still refresh [always] files (e.g. auth tokens)
        copy_always_files "$sprite_name"
        install_open_wrapper "$sprite_name"
    else
        export _SETUP_TARGET_DIR="/home/sprite/$(basename "$repo")"
        export _SETUP_TARGET_DIR_NAME=""
        export _SETUP_CURRENT_DIR=""
        export _SETUP_IS_NEW="$is_new"
        export _SETUP_MODE="repo"
        spin "Setting up sprite..." _do_setup_sprite "$sprite_name"
    fi

    # Sync repository (always pull latest)
    spin "Syncing repository..." _do_sync_repo "$sprite_name" "$repo"

    # Open session in the repo directory
    local repo_dir
    repo_dir=$(basename "$repo")
    local session_name
    session_name=$(derive_session_name)
    msg "Connecting to $repo_dir (tmux: $session_name)"

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

# Get a file's modification time as a unix timestamp (portable across macOS/Linux).
local_file_mtime() {
    if [[ "$OSTYPE" == darwin* ]]; then
        stat -f %m "$1"
    else
        stat -c %Y "$1"
    fi
}

# Copy a single setup.conf file entry to the sprite.
# Supports per-file copy mode via [always] or [newest] annotation.
# Default mode is "newest" — only copy when local file is newer than remote.
#   ~/.tmux.conf                        # newest (default)
#   ~/.tmux.conf [always]               # always overwrite
#   ~/.config/foo -> ~/.bar [newest]    # explicit newest
copy_setup_file() {
    local sprite_name="$1"
    local file_path="$2"

    # Trim whitespace
    file_path=$(echo "$file_path" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')

    # Parse optional [mode] annotation at end of line
    local copy_mode="newest"
    if [[ "$file_path" =~ \[(always|newest)\]$ ]]; then
        copy_mode="${BASH_REMATCH[1]}"
        # Strip the annotation from the path
        file_path=$(echo "$file_path" | sed 's/[[:space:]]*\['"$copy_mode"'\]$//')
    fi

    local local_path remote_path

    # Support "source -> dest" syntax for different local/remote paths
    if [[ "$file_path" == *" -> "* ]]; then
        local source="${file_path%% -> *}"
        local dest="${file_path##* -> }"
        source=$(echo "$source" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        dest=$(echo "$dest" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        local_path="${source/#\~/$HOME}"
        remote_path="${dest/#\~//home/sprite}"
    else
        # Expand ~ for local and remote paths
        local_path="${file_path/#\~/$HOME}"
        remote_path="${file_path/#\~//home/sprite}"
    fi

    if [[ ! -f "$local_path" ]]; then
        warn "Setup file not found locally: $local_path"
        return 0
    fi

    # Decide whether to copy based on mode
    if [[ "$copy_mode" == "newest" ]]; then
        # Get remote mtime; if file doesn't exist or stat fails, remote_mtime is empty
        local remote_mtime
        remote_mtime=$(sprite exec -s "$sprite_name" stat -c %Y "$remote_path" 2>/dev/null) || true

        # Only compare if we got a valid integer back (stat can emit error text to stdout)
        if [[ "$remote_mtime" =~ ^[0-9]+$ ]]; then
            local local_mtime
            local_mtime=$(local_file_mtime "$local_path")
            # Skip if remote is same age or newer
            if [[ "$local_mtime" -le "$remote_mtime" ]]; then
                return 0
            fi
        fi
        # Remote missing, stat failed, or local is newer — fall through to copy
    fi
    # "always" mode — no skip check, always copy

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

    # sprite exec -file uploads as ubuntu:ubuntu and clobbers parent dir
    # ownership via mkdirParents. Fix both the file and its parent directory.
    sprite exec -s "$sprite_name" sh -c \
        "chown sprite:sprite '$remote_path' '$remote_dir' 2>/dev/null; chown sprite:sprite /home/sprite 2>/dev/null" \
        || true
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
#   ~/.local/bin/claude                           # copy if local is newer (default)
#   ~/.config/sprite/foo.conf -> ~/.foo.conf      # different source and dest
#   ~/.tmux.conf [always]                         # always overwrite remote
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

    # sprite exec -file clobbers parent directory ownership to ubuntu:ubuntu.
    # After all setup file copies, fix /home/sprite and ~/.ssh so SSH works.
    if [[ "$has_work" == "true" ]]; then
        sprite exec -s "$sprite_name" sh -c \
            'chown sprite:sprite /home/sprite 2>/dev/null; chown -R sprite:sprite ~/.ssh 2>/dev/null; chmod 700 ~/.ssh 2>/dev/null' \
            || true
    fi
}

# Install a tmux-aware `open` wrapper inside the sprite.
# The sprite CLI uses a custom OSC escape sequence (ESC ] 9999;browser-open;URL ST)
# to open URLs in the user's local browser. When the session runs inside tmux
# (as sp does), tmux swallows unknown OSC sequences before they reach the
# sprite transport. This wrapper detects $TMUX and wraps the OSC in a DCS
# passthrough envelope so it passes through tmux to the sprite client.
# Always reinstalled on connect so we can iterate on the implementation.
install_open_wrapper() {
    local sprite_name="$1"

    # Write the wrapper to a local temp file, upload it via -file, then
    # move it into place and ensure ~/bin is on PATH. Using -file avoids
    # all quoting/escaping issues with nested heredocs and escape sequences.
    local tmp_open="/tmp/sprite-open-wrapper-$$.sh"
    cat > "$tmp_open" <<'OPEN_WRAPPER'
#!/bin/sh
# Sprite-aware open command — emits OSC 9999 to open URLs in the local browser.
# Installed by sp. When inside tmux, wraps the sequence in DCS passthrough.

url="$1"
if [ -z "$url" ]; then
    echo "Usage: open <url>" >&2
    exit 1
fi

# Emit the OSC 9999 browser-open sequence.
# Inside tmux, wrap in DCS passthrough so the escape reaches the sprite client.
if [ -n "$TMUX" ]; then
    # DCS passthrough: \ePtmux; then inner sequence with ESC doubled, then \e\\
    printf '\ePtmux;\e\e]9999;browser-open;%s\a\e\\' "$url"
else
    printf '\e]9999;browser-open;%s\a' "$url"
fi
OPEN_WRAPPER

    sprite exec -s "$sprite_name" \
        -file "${tmp_open}:/tmp/sprite-open-wrapper.sh" \
        sh -c 'mkdir -p ~/bin && mv /tmp/sprite-open-wrapper.sh ~/bin/open && chmod +x ~/bin/open && if ! grep -q "HOME/bin" ~/.profile 2>/dev/null; then echo "export PATH=\"\$HOME/bin:\$PATH\"" >> ~/.profile; fi' \
        2>/dev/null || true

    rm -f "$tmp_open"
}

# Copy files annotated with [always] from setup.conf.
# Called on every connection, even when the sprite is already "ready",
# so that [always] files (like auth tokens) stay current.
copy_always_files() {
    local sprite_name="$1"

    [[ -f "$SPRITE_SETUP_CONF" ]] || return 0

    local copied_any=false
    local section=""
    while IFS= read -r line <&3 || [[ -n "$line" ]]; do
        [[ -z "$line" || "$line" =~ ^[[:space:]]*# ]] && continue

        if [[ "$line" =~ ^\[([a-z]+)\]$ ]]; then
            section="${BASH_REMATCH[1]}"
            continue
        fi

        if [[ "$section" == "files" ]] && [[ "$line" =~ \[always\] ]]; then
            copy_setup_file "$sprite_name" "$line"
            copied_any=true
        fi
    done 3< "$SPRITE_SETUP_CONF"

    # sprite exec -file clobbers parent directory ownership to ubuntu:ubuntu
    # via mkdirParents. After all file copies, fix /home/sprite and ~/.ssh
    # ownership so SSH StrictModes doesn't reject pubkey auth.
    if [[ "$copied_any" == "true" ]]; then
        sprite exec -s "$sprite_name" sh -c \
            'chown sprite:sprite /home/sprite 2>/dev/null; chown -R sprite:sprite ~/.ssh 2>/dev/null; chmod 700 ~/.ssh 2>/dev/null' \
            || true
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

    # Create a temporary tar file of the directory contents.
    # COPYFILE_DISABLE prevents macOS from creating ._* resource fork files.
    #
    # If the directory is a git repo, use `git ls-files` to respect .gitignore
    # and avoid uploading build artifacts, binaries, and other ignored files.
    # Falls back to hardcoded excludes for non-git directories.
    local temp_tar="/tmp/sprite-sync-$$.tar.gz"
    local dir_basename
    dir_basename=$(basename "$current_dir")

    if git -C "$current_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        # Git repo: tar only tracked + untracked-but-not-ignored files.
        # Prefix each path with the directory basename so the tar extracts
        # into /home/sprite/<dir_basename>/ matching the target layout.
        git -C "$current_dir" ls-files -co --exclude-standard \
            | sed "s|^|${dir_basename}/|" \
            | COPYFILE_DISABLE=1 tar -czf "$temp_tar" \
                --no-xattrs \
                -C "$(dirname "$current_dir")" \
                -T - 2>/dev/null || {
            warn "Failed to create archive for initial sync"
            rm -f "$temp_tar"
            return 1
        }
    else
        # Not a git repo: use hardcoded excludes for common build artifacts.
        COPYFILE_DISABLE=1 tar -czf "$temp_tar" \
            --no-xattrs \
            --exclude='node_modules' \
            --exclude='.next' \
            --exclude='dist' \
            --exclude='build' \
            --exclude='.DS_Store' \
            --exclude='._*' \
            --exclude='_build' \
            --exclude='deps' \
            --exclude='.elixir_ls' \
            -C "$(dirname "$current_dir")" \
            "$dir_basename" 2>/dev/null || {
            warn "Failed to create archive for initial sync"
            rm -f "$temp_tar"
            return 1
        }
    fi

    # Upload and extract in sprite (use absolute path for -file).
    # Non-fatal: if the tar is too large for the upload timeout, the sprite
    # still boots and mutagen (if enabled) will sync files incrementally.
    info "Uploading directory contents..."
    sprite exec -s "$sprite_name" -file "${temp_tar}:/home/sprite/sync.tar.gz" \
        sh -c "cd ~ && tar -xzf sync.tar.gz 2>/dev/null && rm sync.tar.gz" || {
        warn "Initial sync upload failed (archive may be too large). Mutagen will sync files if enabled."
        rm -f "$temp_tar"
        return 1
    }

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
    elif [[ -n "$repo" ]]; then
        sprite_name=$(repo_to_sprite_name "$repo")
        target_dir_name=$(basename "$repo")
    else
        sprite_name=$(dir_to_sprite_name "$dir_name")
        target_dir_name="$dir_name"
    fi

    local target_dir="/home/sprite/$target_dir_name"

    # Pre-acquire token before spinners (interactive prompt can't run inside gum spin)
    local claude_token
    claude_token=$(get_claude_token)
    export CLAUDE_CODE_OAUTH_TOKEN="$claude_token"

    # Export state for spinner subprocess
    export _PREFLIGHT_TARGET_DIR="$target_dir"
    export _PREFLIGHT_TARGET_DIR_NAME="$target_dir_name"
    export _PREFLIGHT_CURRENT_DIR="$current_dir"

    # All preflight work under one spinner (existence check, sprite use, create/setup).
    # On the fast path this covers sprite_exists + sprite use (~1-2s).
    # On the slow path it covers creation + full setup.
    local session_name
    session_name=$(derive_session_name)
    spin "Connecting to $target_dir_name (tmux: $session_name)" _do_connect_preflight "$sprite_name"

    # Post-spinner: set up sync state in parent (needed for cleanup trap).
    # Re-checking has_active_sync is cheap (local files + lsof) — the expensive
    # work (create, setup) already happened in the spinner subprocess.
    if [[ "$SYNC_ENABLED" == "true" ]]; then
        SYNC_SPRITE_NAME="$sprite_name"
        SSH_PORT=$(sprite_ssh_port "$sprite_name")

        if has_active_sync "$sprite_name" "$current_dir" "$target_dir"; then
            # Sync already running (possibly recovered), just join.
            # Set session name so cleanup can terminate it if we're the last user.
            SYNC_SESSION_NAME="sprite-${sprite_name}"
            register_sync_user "$sprite_name"
            # Warn about any existing conflicts before user enters the shell
            check_sync_conflicts "$sprite_name"
        else
            # Start sync pipeline in background so user gets a shell immediately
            SYNC_SESSION_NAME="sprite-${sprite_name}"
            register_sync_user "$sprite_name"

            local sync_log="/tmp/sprite-sync-${sprite_name}.log"
            (
                setup_sync_session "$sprite_name" "$current_dir" "$target_dir"
            ) > "$sync_log" 2>&1 &
            SYNC_BG_PID=$!
        fi
    fi

    exec_in_sprite "$sprite_name" "$target_dir" "$claude_token"
}

# Set up cleanup trap
trap cleanup EXIT INT TERM HUP

# Main entry point
main() {
    # Self-invocation dispatch for gum spin subprocesses.
    # spin() runs: "$0" _run func args...
    if [[ "${1:-}" == "_run" ]]; then
        shift; VERBOSE=true; "$@"; exit $?
    fi

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
        info|sessions|resync|status)
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
            --verbose|-v)
                VERBOSE=true
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

    # Install gum for spinner support (non-fatal)
    ensure_gum || true

    # Dispatch subcommands
    case "$subcommand" in
        info)
            handle_info "$target"
            ;;
        sessions)
            handle_sessions "$target"
            ;;
        resync)
            check_mutagen
            handle_resync "$target"
            ;;
        status)
            handle_status "$target"
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
