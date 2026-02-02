# sp - Sprite Repository Manager

A command-line tool for managing Fly.io sprites with GitHub repositories. Automatically handles authentication, repository syncing, and Claude Code CLI sessions.

## Features

- Automatically creates or connects to sprites based on GitHub repository names
- Copies Claude authentication tokens and SSH keys to sprites
- Syncs repositories with `git pull --recurse-submodules`
- Opens Claude Code CLI sessions in the repository directory
- Supports working with local repository changes via directory sync

## Prerequisites

- [Fly.io sprite CLI](https://fly.io/docs/reference/sprites/) installed and authenticated
- Git
- Claude Code CLI (required for opening Claude sessions)
- SSH key at `~/.ssh/id_ed25519` (for GitHub authentication)
- Claude Code OAuth token (will be generated automatically on first run via `claude setup-token`)

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
sp owner/repo
```

This will:
1. Create a sprite named `gh-owner--repo` (if it doesn't exist)
2. Copy your Claude config and SSH keys to the sprite
3. Clone the repository (or pull latest changes if already cloned)
4. Open a Claude Code CLI session in the repository directory

Example:
```bash
sp superfly/flyctl
```

### Work with Current Directory

```bash
cd /path/to/your/repo
sp .
```

This will:
1. Detect the GitHub repository from the current directory's git remote
2. Create or connect to the appropriate sprite
3. Sync your local directory contents to the sprite (excluding `.git/objects`, `node_modules`, etc.)
4. Open a Claude Code CLI session in the synced directory

Note: This syncs your local changes to the sprite, allowing you to work with uncommitted changes in Claude.

## Sprite Naming Convention

Sprites are automatically named using the pattern: `gh-owner--repo`

Examples:
- `superfly/flyctl` → sprite name: `gh-superfly--flyctl`
- `anthropics/claude-code` → sprite name: `gh-anthropics--claude-code`

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

When using `sp .`, the following directories are excluded from sync:
- `.git/objects`, `.git/refs`, `.git/logs` (partial git exclusion to reduce size)
- `node_modules`
- `.next`
- `dist`
- `build`
- `.DS_Store`

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

### "Not a git repository or no origin remote"
Make sure you're in a git repository with a GitHub remote configured:
```bash
git remote -v
```

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

# Quick access to any public GitHub repo
sp torvalds/linux
```

## License

MIT
