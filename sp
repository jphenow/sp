#!/usr/bin/env bash

set -euo pipefail

# sp - Sprite environment manager for GitHub repositories
# Usage:
#   sp owner/repo    - Create or access sprite for a GitHub repository
#   sp .             - Create or access sprite for current directory's repo

# Configuration
CLAUDE_CONFIG_DIR="${HOME}/.claude"
SSH_KEY="${HOME}/.ssh/id_ed25519"
SSH_PUB_KEY="${HOME}/.ssh/id_ed25519.pub"

# Check for required commands
check_requirements() {
    local missing=()

    command -v sprite >/dev/null 2>&1 || missing+=("sprite")
    command -v git >/dev/null 2>&1 || missing+=("git")

    if [[ ${#missing[@]} -gt 0 ]]; then
        error "Missing required commands: ${missing[*]}"
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
get_github_repo() {
    local remote_url
    remote_url=$(git config --get remote.origin.url 2>/dev/null) || error "Not a git repository or no origin remote"

    # Handle both SSH and HTTPS URLs
    # SSH: git@github.com:owner/repo.git
    # HTTPS: https://github.com/owner/repo.git
    if [[ "$remote_url" =~ github\.com[:/]([^/]+)/([^/]+)(\.git)?$ ]]; then
        local owner="${BASH_REMATCH[1]}"
        local repo="${BASH_REMATCH[2]}"
        repo="${repo%.git}"  # Remove .git suffix if present
        echo "${owner}/${repo}"
    else
        error "Could not parse GitHub repository from remote: $remote_url"
    fi
}

# Convert owner/repo to sprite name
repo_to_sprite_name() {
    local repo="$1"
    echo "gh-${repo//\//--}"
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

    # Open Claude session
    local repo_dir=$(basename "$repo")
    info "Opening Claude Code session in $repo_dir..."

    # Get the token to pass as env var
    local claude_token
    claude_token=$(get_claude_token)

    sprite exec -s "$sprite_name" -dir "/home/sprite/$repo_dir" -env "CLAUDE_CODE_OAUTH_TOKEN=${claude_token}" -tty claude
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
sync_local_dir() {
    local sprite_name="$1"
    local repo="$2"
    local current_dir="$3"
    local repo_dir=$(basename "$repo")
    local target_dir="/home/sprite/$repo_dir"

    info "Syncing local directory to sprite..."

    # Create a temporary tar file excluding git objects, node_modules, etc.
    local temp_tar="/tmp/sprite-sync-$$.tar.gz"
    tar -czf "$temp_tar" \
        --no-xattrs \
        --exclude='.git/objects' \
        --exclude='.git/refs' \
        --exclude='.git/logs' \
        --exclude='node_modules' \
        --exclude='.next' \
        --exclude='dist' \
        --exclude='build' \
        --exclude='.DS_Store' \
        -C "$(dirname "$current_dir")" \
        "$(basename "$current_dir")" 2>/dev/null || error "Failed to create archive"

    # Upload and extract in sprite (use absolute path for -file)
    info "Uploading directory contents..."
    sprite exec -s "$sprite_name" -file "${temp_tar}:/home/sprite/sync.tar.gz" \
        sh -c "cd ~ && tar -xzf sync.tar.gz 2>/dev/null && rm sync.tar.gz" || error "Failed to upload and extract"

    # Clean up
    rm -f "$temp_tar"

    # Ensure git directory is intact
    sprite exec -s "$sprite_name" -dir "$target_dir" \
        sh -c "if [ -d .git ]; then git reset --hard HEAD 2>/dev/null || true; fi"

    info "Directory synced to $target_dir"
}

# Main function for current directory mode
handle_current_dir_mode() {
    local current_dir
    current_dir=$(pwd)

    # Get GitHub repo info
    local repo
    repo=$(get_github_repo)

    info "Detected GitHub repository: $repo"
    info "Working directory: $current_dir"

    local sprite_name
    sprite_name=$(repo_to_sprite_name "$repo")

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

    # Check if SSH key exists in sprite, set up auth if missing
    if [[ "$is_new" == "true" ]] || ! sprite exec -s "$sprite_name" test -f /home/sprite/.ssh/id_ed25519 2>/dev/null; then
        setup_sprite_auth "$sprite_name"
        setup_git_config "$sprite_name"
    fi

    # Sync local directory to sprite
    sync_local_dir "$sprite_name" "$repo" "$current_dir"

    # Open Claude session in the synced directory
    local repo_dir=$(basename "$repo")
    info "Opening Claude Code session in $repo_dir..."

    # Get the token to pass as env var
    local claude_token
    claude_token=$(get_claude_token)

    sprite exec -s "$sprite_name" -dir "/home/sprite/$repo_dir" -env "CLAUDE_CODE_OAUTH_TOKEN=${claude_token}" -tty claude
}

# Main entry point
main() {
    check_requirements

    if [[ $# -eq 0 ]]; then
        error "Usage: sp owner/repo OR sp ."
    fi

    local arg="$1"

    case "$arg" in
        .)
            handle_current_dir_mode
            ;;
        */*)
            handle_repo_mode "$arg"
            ;;
        *)
            error "Invalid argument. Use 'owner/repo' format or '.' for current directory"
            ;;
    esac
}

main "$@"
