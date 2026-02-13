# sp - Sprite Repository Manager

A command-line tool for managing Fly.io sprites with GitHub repositories or local directories. Automatically handles authentication, repository syncing, and Claude Code CLI sessions.

## Features

- Automatically creates or connects to sprites based on GitHub repository names
- Works with any local directory, even without a GitHub repository
- Copies Claude authentication tokens and SSH keys to sprites
- Syncs repositories with `git pull --recurse-submodules`
- Opens persistent tmux sessions in the repository directory
- Bidirectional file syncing with Mutagen (on by default, disable with `--no-sync`)
- Configurable setup: auto-copy files and run commands on first connect via `~/.config/sprite/setup.conf`

## Prerequisites

- [Fly.io sprite CLI](https://fly.io/docs/reference/sprites/) installed and authenticated
- Git
- Claude Code CLI (required for opening Claude sessions)
- SSH key at `~/.ssh/id_ed25519` (for GitHub authentication)
- Claude Code OAuth token (will be generated automatically on first run via `claude setup-token`)
- [Mutagen](https://mutagen.io/) (required for bidirectional sync, disable with `--no-sync`): `brew install mutagen`

## Installation

### Option 1: Add to PATH

```bash
# Clone this repository
git clone https://github.com/jphenow/sp.git
cd sp

# Make executable
chmod +x sp

# Add to your PATH (example for ~/.local/bin)
ln -s $(pwd)/sp ~/.local/bin/sp
```

### Option 2: Shell Alias

Add to your `~/.bashrc` or `~/.zshrc`:

```bash
alias sp='/path/to/sprite-repo/sp'
```

## Usage

### Work with a GitHub Repository

```bash
sp owner/repo [--no-sync] [--name NAME] [-- COMMAND...]
```

This will:
1. Create a sprite named `gh-owner--repo` (if it doesn't exist)
2. Set up authentication, git config, and run setup.conf entries
3. Clone the repository (or pull latest changes if already cloned)
4. Start bidirectional file sync via Mutagen
5. Open a tmux session running the command (default: `bash`) in the repository directory

Example:
```bash
sp superfly/flyctl
```

### Work with Current Directory

```bash
cd /path/to/your/project
sp . [--no-sync] [--name NAME] [-- COMMAND...]
```

This will:
1. Detect the GitHub repository from the current directory's git remote (if available)
2. If no GitHub repo is found, use the directory name with a `local-` prefix
3. Create or connect to the appropriate sprite
4. Sync your local directory contents to the sprite
5. Open a tmux session running the command (default: `bash`) in the synced directory

Works with any directory -- no GitHub repository required.

### Custom Commands (`--`)

By default, `sp` launches `bash` inside the sprite. Use `--` to run a different command:

```bash
# Open claude instead of bash
sp . -- claude

# Run a command with flags
sp . -- claude -f foo bar baz

# Alternative: use --cmd
sp owner/repo --cmd vim
```

### Custom Session Names (`--name`)

By default, the tmux session is named after the command (e.g., `bash`). Use `--name` to set a custom session name:

```bash
# Launch bash in a session named "feature"
sp . --name feature

# Launch claude and bash simultaneously in the same sprite
sp . -- claude --name claude-session   # Terminal 1
sp . --name debug                      # Terminal 2
```

### tmux Session Behavior

Each `sp` invocation runs its command inside a persistent tmux session within the sprite. This provides:

- **Persistence** — if you disconnect or your terminal closes, the session keeps running. Reconnecting with the same `sp` command reattaches to the existing session.
- **Detach/reattach** — press `Ctrl-b d` inside tmux to detach without stopping the session. Run `sp` again to reattach.
- **Multiple sessions** — use `--name` to run multiple independent sessions in the same sprite (e.g., `claude` + `bash` side by side).
- **Command exits = session exits** — if the command finishes (e.g., `claude -c` with no conversation), the session ends. Use interactive commands for persistence.

### Help

```bash
sp --help
sp -h
```

Shows full usage information including all options and examples.

### Sprite Info (`sp info`)

Inspect sprite metadata without connecting:

```bash
sp info .
sp info owner/repo
sp info owner/repo --cmd bash --name debug
```

Prints sprite name, existence, repository, target directory, command, and session name.

### List Sessions (`sp sessions`)

List active tmux sessions in a sprite:

```bash
sp sessions .
sp sessions owner/repo
```

### Bidirectional File Syncing

Sync is **on by default**. Real-time bidirectional file syncing via Mutagen keeps your local files and the sprite in lockstep:

- Changes you make locally are automatically synced to the sprite
- Changes made in the sprite are automatically synced back to your machine
- Sync session is active while `sp` is running
- Automatic cleanup when you exit

**Prerequisites:**
- Mutagen installed locally: `brew install mutagen`
- SSH key at `~/.ssh/id_ed25519` (used for SSH authentication to sprite)

**What gets synced:**
All files including `.git`, except:
- `node_modules` (dependencies)
- `.next`, `dist`, `build` (build artifacts)
- `.DS_Store`, `._*` (system files)

**Disable sync:**
```bash
sp . --no-sync
```

**Example workflow:**
```bash
cd ~/projects/my-app
sp .

# Edit locally → changes appear in sprite instantly
# Edit in sprite → changes appear locally instantly
# Exit → sync stops and cleans up
```

### Setup Config (`sp conf`)

Manage `~/.config/sprite/setup.conf` to auto-copy files and run commands on first connect:

```bash
sp conf init    # Create starter config
sp conf edit    # Edit in $EDITOR
sp conf show    # Print contents
```

**Config format:**
```ini
[files]
# Copy files to the sprite. ~ expands to $HOME locally, /home/sprite remotely.
# Files are only copied if they don't already exist on the sprite.
~/.tmux.conf
~/.local/share/opencode/auth.json

# Use "source -> dest" when local and remote paths differ:
~/.config/sprite/tmux.conf.local -> ~/.tmux.conf.local

[commands]
# condition :: command — runs on the sprite when condition succeeds (exits 0).
! command -v opencode :: curl -fsSL https://opencode.ai/install | bash
```

Setup runs once per sprite (cached in `/tmp`). Edit setup.conf to re-trigger on next connect.

## Sprite Naming Convention

Sprites are named using the following priority order:

### 1. `.sprite` file (highest priority)

If a `.sprite` file exists in the current directory, `sp` reads the sprite name from it:

```json
{
  "organization": "your-org",
  "sprite": "gh-owner--repo"
}
```

This is useful when you want to use a specific sprite that differs from the auto-detected name, or when working with `sprite use` to set a default.

### 2. GitHub remote

If the current directory is a git repo with a GitHub remote, the sprite is named `gh-owner--repo`:
- `superfly/flyctl` → `gh-superfly--flyctl`
- `anthropics/claude-code` → `gh-anthropics--claude-code`

### 3. Directory name (fallback)

If no `.sprite` file exists and no GitHub remote is detected, the directory name is used with a `local-` prefix:
- `~/projects/my-app` → `local-my-app`
- `~/scratch/experiment` → `local-experiment`

## Authentication

The tool automatically sets up authentication in sprites:

### Claude Code Authentication
- On first run, the script will prompt you to generate a token
- You'll need to run `claude setup-token` in a separate terminal
- Copy and paste the token when prompted
- The token is stored in `~/.claude-token` for future use
- Sets `CLAUDE_CODE_OAUTH_TOKEN` environment variable in sprite shell

### SSH Keys
- Copies `~/.ssh/id_ed25519` (private key)
- Copies `~/.ssh/id_ed25519.pub` (public key)
- Configures SSH for GitHub with `StrictHostKeyChecking accept-new`

## Directory Sync Exclusions

When using `sp .`, the following are excluded from Mutagen sync:
- `node_modules`
- `.next`
- `dist`
- `build`
- `.DS_Store`
- `._*`

Note: `.git` is **included** in sync to keep branches in lockstep between local and sprite.

## Token Setup

On first run, `sp` will prompt you to set up authentication:

1. The script will pause and show instructions
2. Open a new terminal and run `claude setup-token`
3. Copy the token (starts with `sk-ant-oat01-`)
4. Paste it back into the `sp` prompt
5. The token is saved to `~/.claude-token` and reused for all sprites

Alternatively, you can set up the token beforehand:

```bash
# Generate a token
claude setup-token

# Copy the token and save it
echo "sk-ant-oat01-YOUR_TOKEN_HERE" > ~/.claude-token
chmod 600 ~/.claude-token
```

## Troubleshooting

### "sprite: command not found"
Install the Fly.io sprite CLI: https://fly.io/docs/reference/sprites/

### SSH authentication issues
Ensure your SSH key is added to your GitHub account:
```bash
ssh -T git@github.com
```

## Examples

```bash
# Work on the flyctl repository
sp superfly/flyctl

# Work on your local changes before committing
cd ~/projects/my-app
sp .

# Work on a directory without a GitHub repo
cd ~/scratch/prototype
sp .

# Quick access to any public GitHub repo
sp torvalds/linux

# Open a bash shell in a sprite
sp . --cmd bash

# Run claude and bash simultaneously in the same sprite
sp . --name claude-session        # Terminal 1
sp . --cmd bash --name debug      # Terminal 2

# Check sprite info before connecting
sp info owner/repo

# List active tmux sessions
sp sessions .

# Sync + custom session name
sp . --sync --cmd claude --name feature
```

## License

MIT
