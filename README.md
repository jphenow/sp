# sp — Sprite Repository Manager

`sp` gives you isolated cloud dev environments powered by [Fly.io Sprites](https://sprites.dev) with real-time bidirectional file sync, persistent tmux sessions, and a TUI dashboard. Edit locally, run remotely — your files stay in lockstep.

## Quickstart

### 1. Install the Sprite CLI and authenticate

```bash
curl -fsSL https://sprites.dev/install | sh
sprite login
```

### 2. Install sp

Requires Go 1.21+:

```bash
go install github.com/jphenow/sp@latest
```

Or clone and build:

```bash
git clone https://github.com/jphenow/sp.git
cd sp
make install
```

### 3. Install Mutagen (for file sync)

```bash
brew install mutagen-io/mutagen/mutagen
```

### 4. Boot a sprite with the web UI

```bash
cd ~/projects/my-app
sp . --web
```

This creates a sprite for your project, syncs your files, and prints a URL where opencode's web UI is running. The sprite auto-wakes on HTTP access — no need to keep a terminal open.

That's it. Your local edits sync to the sprite in real-time, and the opencode web UI is accessible from your browser.

---

## Why sp?

**Real-time bidirectional file sync.** Edit in your local editor, run in the cloud. Changes flow both ways instantly via [Mutagen](https://mutagen.io) in `two-way-safe` mode — neither side silently overwrites the other.

**`.gitignore`-aware.** All `.gitignore` patterns (including nested ones) are automatically converted to Mutagen exclusion rules. `node_modules`, `_build`, `dist` — all excluded without configuration.

**Persistent sessions.** Everything runs inside tmux. Disconnect and reconnect freely — your processes keep running. The daemon manages sync lifecycle independently, starting and stopping as sprites wake and sleep.

**Auto-wake.** Sprites go to sleep when idle and wake automatically on access. The `--web` flag configures a sprite-env service so your web UI wakes the sprite on HTTP request.

**Dashboard.** `sp tui` gives you a live view of all your sprites — status, sync health, tags — with keyboard shortcuts to open, connect, sync, and delete.

---

## Core Workflows

### Connect to a sprite (console)

```bash
# From a git repo — sprite name derived from GitHub remote
cd ~/projects/my-app
sp .

# Or specify owner/repo directly
sp superfly/flyctl

# Run a specific command instead of bash
sp . -- claude
```

This creates the sprite if needed, sets up auth and SSH keys, starts file sync, and drops you into a tmux session.

### Boot the web UI

```bash
# Direct mode — opencode web UI accessible at the sprite's public URL
sp . --web

# Proxy mode — /opencode routes to opencode, /* falls through to your dev server
sp . --web --web-proxy --web-dev-port 3000
```

In `--web` mode, `sp` configures a sprite-env service and returns immediately. The daemon keeps sync running in the background.

### Monitor with the TUI

```bash
sp tui
```

The dashboard shows all tracked sprites with live status and sync health. Key bindings:

| Key | Action |
|-----|--------|
| `j/k` or `↑/↓` | Navigate |
| `enter` | View details |
| `o` | Open sprite URL in browser |
| `c` | Connect via console (suspends TUI) |
| `s` | Start/stop sync |
| `d` | Delete sprite (with confirmation) |
| `t` / `T` | Add / remove tag |
| `f` | Filter by name |
| `r` | Refresh |
| `q` | Quit |

### Run with proxy mode

For projects with a dev server, proxy mode puts opencode and your app behind a single URL:

```bash
sp . --web --web-proxy --web-dev-port 4000
```

This uploads the `sp` binary to the sprite and runs a reverse proxy:
- `/opencode` routes to the opencode web UI
- `/*` routes to your dev server on port 4000

### Connect to a sync'd console session

When `--web` is running, your files are already syncing. Open a console alongside it:

```bash
# Open a bash session in the same sprite
sp .

# Or run claude directly
sp . -- claude
```

The daemon manages sync — it persists across all your sessions. Multiple terminal windows can connect to the same sprite simultaneously using `--name`:

```bash
sp . -- claude --name claude-main    # Terminal 1
sp . --name debug                    # Terminal 2
sp . --name tests                    # Terminal 3
```

---

## File Sync

### How it works

`sp` uses [Mutagen](https://mutagen.io) for bidirectional file sync between your local machine and the sprite. A background daemon manages the sync lifecycle:

1. **On connect**, the daemon starts an SSH proxy to the sprite and creates a Mutagen session
2. **While running**, changes flow both ways in real-time
3. **When the sprite sleeps**, sync stops cleanly and restarts automatically when the sprite wakes
4. **The daemon persists** — sync survives after `sp` exits, across terminal sessions, and through sprite sleep/wake cycles

### Sync mode

Sync uses `two-way-safe` — if both sides modify the same file before syncing, Mutagen flags a conflict instead of choosing a winner. Check with `sp status .` and resolve with `mutagen sync reset`.

### What gets synced

Everything except:
- Files matching `.gitignore` patterns (all `.gitignore` files in the tree are parsed)
- `.DS_Store` and `._*` files (always excluded)
- `.git/` is **included** so branch state stays in lockstep

### Managing sync

```bash
# Check sync health and conflicts
sp status .

# Reset sync (flush pending changes, re-read .gitignore, restart)
sp resync .

# List active tmux sessions
sp sessions .

# Discover and import existing Mutagen sessions
sp discover
```

---

## Setup Configuration

`sp` automatically configures each sprite with:
- Claude Code OAuth token (from `~/.claude-token` or interactive prompt)
- SSH keys (`~/.ssh/id_ed25519`) for GitHub access
- Git user name and email
- OSC browser-open wrapper for tmux passthrough

### Custom files and commands

Create `~/.config/sprite/setup.conf` to auto-install tools and copy dotfiles:

```bash
sp conf init    # Create starter config with examples
sp conf edit    # Open in $EDITOR
sp conf show    # Print current config
```

**Config format:**

```ini
[files]
# Copy dotfiles. Default mode: [newest] (only if local is newer).
~/.tmux.conf
~/.config/opencode/config.toml [always]

# Different local/remote paths:
~/.config/sprite/tmux.conf.local -> ~/.tmux.conf.local

[commands]
# Conditional: runs when condition succeeds (exits 0)
! command -v opencode :: curl -fsSL https://opencode.ai/install | bash
command -v npm :: npm install -g prettier
```

**How it runs:**
- `[files]` entries copy local files to the sprite. `[newest]` (default) skips if the remote is the same age or newer. `[always]` overwrites every time.
- `[commands]` entries run on the sprite. The condition before `::` is checked first — the command only runs if the condition passes. Use `!` prefix to negate (run when condition fails).
- Setup runs on first connect to a new sprite. Files marked `[always]` are re-copied on every reconnect.

---

## The Daemon

`sp` runs a background daemon that manages sync lifecycle, tracks sprite health, and serves the TUI. It starts automatically on first use.

```bash
sp daemon status    # Check if daemon is running
sp daemon restart   # Restart (picks up new binary automatically)
sp daemon logs      # Show recent log output
sp daemon logs -f   # Follow logs in real-time
```

**Auto-restart:** The daemon checks its own binary hash every 10 seconds. When you rebuild and `make install`, the daemon and TUI automatically re-exec with the new code.

**State:** Sprite metadata, sync sessions, and tags are stored in `~/.config/sp/sp.db` (SQLite). Logs go to `~/.config/sp/sp.log`.

---

## Command Reference

| Command | Description |
|---------|-------------|
| `sp .` | Connect to sprite for current directory |
| `sp owner/repo` | Connect to sprite for a GitHub repo |
| `sp . --web` | Set up opencode web UI with auto-wake |
| `sp . --web --web-proxy` | Proxy mode (opencode + dev server) |
| `sp tui` | Open the dashboard |
| `sp status [target]` | Show sprite and sync status |
| `sp setup [target]` | Re-run setup.conf on a sprite |
| `sp setup --all` | Re-run setup.conf on all tracked running sprites |
| `sp resync [target]` | Reset file sync |
| `sp sessions [target]` | List tmux sessions |
| `sp import <name>` | Import an existing sprite |
| `sp discover` | Find and import untracked Mutagen sessions |
| `sp conf init/edit/show` | Manage setup.conf |
| `sp daemon status/restart/logs` | Manage the background daemon |

### Flags

| Flag | Description |
|------|-------------|
| `--web` | Enable opencode web UI via sprite-env service |
| `--web-proxy` | Reverse proxy mode (requires `--web`) |
| `--web-dev-port N` | Dev server port for proxy fallthrough |
| `--no-sync` | Disable file syncing |
| `--name NAME` | Custom tmux session name |
| `-- COMMAND` | Run a command instead of bash |

---

## Sprite Naming

Sprites are named automatically based on what you connect to:

| Source | Example | Sprite Name |
|--------|---------|-------------|
| `.sprite` file | `{"sprite": "my-sprite"}` | `my-sprite` |
| GitHub remote | `superfly/flyctl` | `gh-superfly--flyctl` |
| Directory name | `~/projects/my-app` | `local-my-app` |

Priority: `.sprite` file > GitHub remote > directory name.

---

## Prerequisites

| Tool | Required for | Install |
|------|-------------|---------|
| [Sprite CLI](https://sprites.dev) | Core functionality | `curl -fsSL https://sprites.dev/install \| sh` |
| [Go](https://go.dev) | Building sp | `brew install go` |
| [Mutagen](https://mutagen.io) | File sync | `brew install mutagen-io/mutagen/mutagen` |
| SSH key | GitHub + sync | `ssh-keygen -t ed25519` |
| Claude Code token | Claude integration | `claude setup-token` (prompted on first run) |

---

## Troubleshooting

### Sync not working

```bash
sp status .          # Check sync health
sp resync .          # Full reset
sp daemon logs -n 20 # Check daemon logs for errors
```

### Sprite won't connect

```bash
sprite list          # Verify sprite exists
sp daemon status     # Verify daemon is running
sp daemon restart    # Restart daemon
```

### Stale SSH config entries

The daemon cleans up stale SSH config entries (`~/.ssh/config`) on startup. If entries accumulate, restart the daemon:

```bash
sp daemon restart
```

### Mouse tracking garbage after disconnect

If scrolling produces raw escape sequences after disconnecting from a sprite, `sp` automatically resets terminal mouse modes. If this still happens (e.g., after a crash), run:

```bash
printf '\e[?1000l\e[?1003l\e[?1006l'
```

---

## License

MIT
