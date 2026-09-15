# sp — Sprite Repository Manager

`sp` turns a local directory or a GitHub repository into a cloud dev environment on a [Fly.io Sprite](https://sprites.dev). It creates and names the sprite, pushes your Claude Code, GitHub, git, and SSH credentials, keeps your local files in two-way sync with [Mutagen](https://mutagen.io), and drops you into a persistent tmux session. A background daemon tracks your sprites and a TUI (`sp tui`) shows them all.

## Requirements

| Tool | Used for |
|------|----------|
| [Sprite CLI](https://sprites.dev) (`sprite login`) | Everything. `sp` drives the `sprite` CLI; it does not call the API directly. |
| [Go](https://go.dev) 1.25+ | Building `sp` |
| [Mutagen](https://mutagen.io) | File sync for local-directory targets |
| `~/.ssh/id_ed25519` (+ `.pub`) | Pushed to the sprite; the public key authorizes Mutagen's SSH connection |
| Claude Code login or `~/.claude-token` | Claude auth on the sprite (see [Claude Code on the sprite](#claude-code-on-the-sprite)) |
| `gh` (optional) | `gh auth token` supplies `GH_TOKEN` for git and gh on the sprite |

## Install

```bash
curl -fsSL https://sprites.dev/install | sh
sprite login

brew install mutagen-io/mutagen/mutagen

git clone https://github.com/jphenow/sp.git
cd sp
make install      # go install . -> $(go env GOPATH)/bin/sp
```

`make build` produces `./sp-bin` instead, if you'd rather alias that.

## Quickstart

```bash
cd ~/projects/my-app
sp .                          # create/wake the sprite, sync this dir, open tmux

sp superfly/flyctl            # a GitHub repo: cloned on the sprite, no local sync
sp . --rc                     # start Claude with Remote Control instead of bash
sp . -- claude                # run any command instead of bash
sp tui                        # dashboard of every tracked sprite
```

---

## Connecting

`sp <target> [variant]` is shorthand for `sp connect <target> [variant]`. The shorthand only recognizes `.` or a target containing `/` (`./dir`, `/abs/path`, `owner/repo`); for a bare sprite name use `sp connect <name>`.

### Targets and sprite names

| Target | Sprite name | Remote path | Files |
|--------|-------------|-------------|-------|
| Directory with a `.sprite` file (`{"organization": "...", "sprite": "..."}`) | the `sprite` value | `/home/sprite/<dirname>` | Synced |
| Directory whose `origin` is on GitHub | `gh-<owner>--<repo>` | `/home/sprite/<repo>` | Synced |
| Any other directory | `local-<dirname>` | `/home/sprite/<dirname>` | Synced |
| `owner/repo` | `gh-<owner>--<repo>` | `/home/sprite/<repo>` | Cloned on the sprite, not synced |
| `<name>` (via `sp connect`) | `<name>` | `/home/sprite` | Neither |

Directory names (for `local-`) and variant labels are lowercased, with spaces, `.`, `_` and `:` replaced by `-`.

### What a connect does

1. Checks whether the sprite exists, creating it if not, then probes it until it answers (up to 3 minutes; a cold sprite takes about 30s to wake).
2. Runs setup in parallel:
   - **Auth**: SSH key and a GitHub `Host` entry in `~/.ssh/config`; Claude credentials (see below); gh `config.yml`; a `~/bin/open` wrapper; and for `owner/repo` targets, `git clone --recurse-submodules`.
   - **Git**: your `user.name` / `user.email`, a `git@github.com:` to `https://github.com/` rewrite, and a credential helper that reads `GH_TOKEN`.
   - **Claude config**: allowlisted entries from `~/.claude/` (`CLAUDE.md`, `settings.json`, `settings.local.json`, `keybindings.json`, `statusline-command.sh`, `commands`, `skills`, `plugins`, `agents`, `marketplace(s)`) and bypass-permissions defaults in the sprite's `settings.json`. Session state (`projects`, `history.jsonl`, and so on) is never pushed.
   - **setup.conf**: on a new sprite, all of `~/.config/sprite/setup.conf` plus an initial upload of the local directory; on an existing sprite, only `[always]` files.
3. Registers the sprite with the daemon, which starts file sync.
4. Starts a hold that keeps the sprite Active while the session lives (see [Keeping a sprite awake](#keeping-a-sprite-awake)).
5. Reattaches to the sprite's existing tmux session if there is one, otherwise creates one named after `--name` or the command (`bash` by default) in the remote path.

A failed setup task prints a warning with its full error and the connect continues to the shell. A progress display shows each task; calls to the sprite CLI that have been running for 3s or more are listed live beneath it.

`GH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` are passed as exec environment and `tmux setenv`, not written to rc files, so they don't persist on a stopped sprite. Panes that already exist keep their old environment; new panes and windows get the refreshed values.

### Sessions

There is one persistent tmux session per sprite. Connecting again, from any terminal, reattaches to it, so `--name`, `--exec` and `--rc` only take effect when `sp` creates the session. Use tmux windows and panes for parallel work, or a variant for a separate sprite.

The session starts an `ssh-agent`; run `ssh-add` once if you need your key for SSH beyond git (git uses HTTPS with `GH_TOKEN`).

### Variants

A second positional argument creates a parallel sprite for the same base:

```bash
sp . scratch-idea               # gh-owner--repo--scratch-idea
sp owner/repo new-approach
```

A variant of a directory gets a one-time upload when it's created (the files `git ls-files -co --exclude-standard` lists, so no `.git`) and no ongoing sync, so it can't fight the base sprite's sync. An `owner/repo` variant is a fresh clone. Variants can be listed, pinned and cleaned up:

```bash
sp status --variants
sp status --variants-of gh-owner--repo
sp pin owner/repo:scratch-idea      # prune skips pinned variants
sp unpin owner/repo:scratch-idea
sp prune                            # dry run: unpinned variants not updated in 14 days
sp prune --older-than 72h --yes     # destroy them (asks again on a TTY)
sp rm owner/repo:scratch-idea       # destroy one; non-variants and pinned need --force
```

### Keeping a sprite awake

Sprites pause when idle. `sp` holds them Active through the sprite Tasks API with a heartbeat that runs on the sprite:

| Mode | Behavior |
|------|----------|
| Default | Held while the tmux session exists, released a minute or so after it ends, capped at 8 hours. |
| `--keep-warm 1h` | Replaces the default hold: held for up to the duration, released early once the session's pane is unchanged for about 60s. |
| `--no-hold` | No hold; the sprite may pause while you're away. |
| `sp keepalive <target> [variant] --for 2h` | Explicit hold for a fixed window, independent of any session. `--stop` releases it. |

A newer hold replaces an older one on the same sprite.

---

## Claude Code on the sprite

### Authentication

`sp` gathers two credentials on every connect:

- **Full-scope login**: your local Claude Code credentials, from the macOS Keychain (`Claude Code-credentials`) or `~/.claude/.credentials.json` on Linux. This is the only kind Remote Control accepts.
- **Setup token**: `CLAUDE_CODE_OAUTH_TOKEN` from your environment, or `~/.claude-token` (from `claude setup-token`). Inference only, but long-lived. If neither credential exists, `sp` prompts for a token and saves it to `~/.claude-token`.

The local credentials file is installed on the sprite only if it expires later than the sprite's own copy, so a credential the sprite has already refreshed isn't overwritten with a staler one. `sp` then runs `claude auth status` on the sprite with `CLAUDE_CODE_OAUTH_TOKEN` unset:

- If it reports a working `claude.ai` login, `CLAUDE_CODE_OAUTH_TOKEN` is **not** injected, and is removed from the tmux environment.
- Otherwise `sp` injects the setup token and prints a note that inference will work but Remote Control won't until you `/login` on the sprite.

This matters because `CLAUDE_CODE_OAUTH_TOKEN` takes precedence over `credentials.json`: when set, it masks the full-scope login, which breaks Remote Control and makes a `/login` on the sprite appear not to stick.

`sp` also unsets `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and the Bedrock/Vertex/Foundry switches in the sprite's shell rc files and tmux environment, and marks onboarding and bypass-permissions consent as done. `claude` on the sprite runs with permissions bypassed.

### Remote Control

```bash
sp . --rc                    # alias: --remote-control
```

Runs `claude --remote-control` as the session command (unless `--exec` is given) and prefixes the Remote Control session name with the sprite name, so you can find it in the Claude app or claude.ai/code. As with other session flags, it only applies when the tmux session is being created. The default session hold keeps the sprite reachable while you're away from the terminal.

For a headless, self-restarting setup:

```bash
sp rc .                      # sprite-env service "sp-rc", sprite held Active for 1h
sp rc owner/repo --for 3h
sp rc . --stop               # remove the service and release the hold
```

The service resumes the last session with `--continue` (or starts a fresh one) and re-registers after a cold wake. `sp rc` requires the sprite to already exist and to have a full `claude.ai` login. It is experimental.

---

## Web mode

```bash
sp . --web                                  # opencode web UI on the sprite URL
sp . --web --web-proxy --web-dev-port 3000  # /opencode -> opencode, /* -> port 3000
```

`--web` installs opencode if needed, creates a sprite-env service with an HTTP port (so HTTP requests wake the sprite), registers with the daemon (required in this mode), prints the URL and exits without opening a shell.

`--web-proxy` cross-compiles `sp` for linux/amd64 from the Go source this `sp` was built from (your checkout, or the module cache for `go install`), uploads it to `/usr/local/bin/sp`, and runs `sp serve` as the service. It needs Go installed and that source still present.

---

## File sync

Directory targets (except variants) are synced by the daemon with Mutagen in `two-way-safe` mode: a file changed on both sides becomes a conflict instead of being overwritten. Mutagen connects over `sprite proxy` to an `sshd` that `sp` installs on the sprite, using a `sprite-mutagen-<name>` entry it adds to `~/.ssh/config`. The Mutagen session is named `sprite-<name>`.

Excluded from sync:

- `node_modules`, `.next`, `dist`, `build`, `.DS_Store`, `._*`
- Patterns from every `.gitignore` in the tree, read when the session is created
- Transient git state: lock files, `rebase-merge`, `rebase-apply`, `sequencer`, bisect files, `gc.log`

`.git` itself is synced, so branches, the index and commits stay in step on both sides.

The daemon polls sprite status every 60s. When a sprite pauses it stops sync; when it's running again it restarts it. While sync is healthy the daemon resets the Mutagen session every 5 minutes to force a rescan, and it recovers sessions stuck connecting.

```bash
sp status gh-owner--repo   # stored status plus live Mutagen state and conflict count
sp resync .                # flush, tear down and restart sync (re-reads .gitignore)
sp discover                # import untracked sprite-* Mutagen sessions
```

To resolve conflicts, use the sync menu in `sp tui` (`s`), which can force or safely push or pull in one direction before returning to two-way sync.

---

## setup.conf

`~/.config/sprite/setup.conf` lists extra files and commands for your sprites.

```bash
sp conf init    # write a commented starter file
sp conf edit    # open in $EDITOR (vi if unset)
sp conf show
```

```ini
[files]
# Mode defaults to [newest]: upload only if the local file is newer than the remote one.
~/.tmux.conf
~/.config/opencode/config.toml [always]
~/.config/sprite/tmux.conf.local -> ~/.tmux.conf.local

[commands]
# condition :: command   (a leading "! " runs the command when the condition fails)
! command -v opencode :: curl -fsSL https://opencode.ai/install | bash
command -v npm :: npm install -g prettier
npm --version
```

- `~/` expands to your home locally and to `/home/sprite` remotely. The executable bit is preserved.
- A line without ` :: ` is a command that always runs. Commands run with `sh -c`.
- Uploads are retried, and a failed batch falls back to per-file uploads, so a failure names the file that didn't make it. Failures are warnings, not errors.

When it runs:

| When | What |
|------|------|
| Connect, new sprite | All files (respecting mode) and all commands |
| Connect, existing sprite | `[always]` files only |
| `sp setup [target]` or `u` in the TUI | All files and commands, in the background via the daemon (output in `sp daemon logs`) |
| `sp setup --all` or `U` in the TUI | The same, for every tracked sprite with status `running` |

Editing `setup.conf` does not by itself cause anything to re-run.

---

## The daemon

The daemon starts automatically the first time a command needs it.

- State: `~/.config/sp/sp.db` (SQLite). Log: `~/.config/sp/sp.log`. Socket and PID file: `~/.config/sp/sp.sock`, `sp.pid`.
- Every 10s it checks whether its own binary has changed on disk and re-execs if so. The TUI does the same.
- It removes stale `sp`-managed entries from `~/.ssh/config` on startup.
- It exits after 10 minutes with no connected client (an open `sp tui` counts; a finished `sp .` doesn't), stopping its sync proxies. The next `sp` command starts it again; its health checks usually bring sync back, and `sp resync .` forces it.

```bash
sp daemon status
sp daemon restart          # re-exec (starts it if not running)
sp daemon logs -n 100      # -f to follow
```

`sp daemon stop` shuts the daemon down gracefully (stopping its sync proxies) and waits for it to exit. `sp daemon start` runs it in the foreground.

---

## TUI

```bash
sp tui
sp tui --filter work,experiments    # only sprites with these tags
sp tui --prefix ~/projects/client   # only sprites whose local path starts with this
```

| Key | Action |
|-----|--------|
| `j`/`k`, arrows | Navigate |
| `enter` | Details |
| `o` | Open the sprite URL in a browser |
| `c` | Open a shell in a separate `console` tmux session on the sprite |
| `s` | Sync menu: start/stop, force push/pull, safe push/pull |
| `u` / `U` | Run setup.conf on this sprite / all running sprites |
| `d` | Stop sync and remove the sprite from sp's database (does not destroy it) |
| `t` / `T` | Add / remove a tag |
| `f` | Filter by name |
| `r` | Refresh |
| `q` | Quit |

Only sprites `sp` knows about appear: ones you've connected to, or added with `sp import <name> [--path DIR] [--tag a,b]` or `sp discover`.

---

## Command reference

| Command | Description |
|---------|-------------|
| `sp <target> [variant]` | Connect (shorthand; target must be `.`, a path, or `owner/repo`) |
| `sp connect [target] [variant]` | Connect; target defaults to `.` and may be a bare sprite name |
| `sp keepalive [target] [variant]` | Hold a sprite Active (`--for`, default 1h; `--stop`) |
| `sp rc [target] [variant]` | Run Claude Remote Control as a sprite service (`--for`, default 1h; `--stop`) |
| `sp status [sprite-name]` | All tracked sprites, or one in detail (`--variants`, `--variants-of`) |
| `sp resync [target]` | Restart file sync |
| `sp sessions [target]` | `sprite sessions list` for the sprite |
| `sp setup [target]` | Re-run setup.conf (`--all` for every running sprite) |
| `sp conf init\|edit\|show` | Manage setup.conf |
| `sp tui` | Dashboard (`--filter`, `--prefix`) |
| `sp import <name>` | Track an existing sprite (`--path`, `--tag`) |
| `sp discover` | Import untracked `sprite-*` Mutagen sessions |
| `sp pin` / `sp unpin <sprite-or-variant>` | Protect a variant from `sp prune` |
| `sp prune` | List stale unpinned variants (`--older-than`, `--all`, `--yes` to destroy) |
| `sp rm <sprite-or-variant>` | Destroy and untrack (`--force` for non-variants or pinned) |
| `sp daemon start\|stop\|status\|restart\|logs` | Manage the daemon |
| `sp serve` | The reverse proxy used by `--web-proxy`; runs on the sprite |
| `sp completion <shell>` | Shell completion script |

`<sprite-or-variant>` is a sprite name or `owner/repo:variant`.

### Connect flags

| Flag | Description |
|------|-------------|
| `--rc`, `--remote-control` | Start `claude --remote-control` as the session command |
| `--name NAME` | tmux session name (default: first word of the command) |
| `--exec CMD` | Command to run instead of `bash` (on `sp connect`). Everywhere, `-- CMD...` after the target does the same: `sp . -- claude --continue`. |
| `--no-sync` | Don't sync: no initial upload, and the directory isn't registered with the daemon for ongoing sync. Sync an earlier connect already set up keeps running (stop it from the TUI sync menu). |
| `--keep-warm DURATION` | Hold with idle-exit instead of the session-tied hold |
| `--no-hold` | Don't hold the sprite Active |
| `--web`, `--web-proxy`, `--web-dev-port N` | See [Web mode](#web-mode) |
| `-v`, `--verbose` | Line-by-line progress, clone output, and the timing profile on every connect |

Session flags (`--name`, `--exec`, `--rc`) are ignored when reattaching to an existing session.


---

## Troubleshooting

### A connect is slow or looks stuck

The live list under the progress display names any sprite CLI call running for 3s or more. If setup takes 45s or longer (or with `-v`), `sp` prints a profile when it finishes: the sprite's status at the start, the number of CLI calls, and the slowest ones. A cold sprite accounts for about 30s on its own.

When a single call takes 5s or more and one pause in the sprite CLI's `--debug` log accounts for at least half of it, the call's line in the profile is annotated:

```
2m15s stalled between "sprites: control disabled by client option" and "Command started"
```

A long stall ending at `Command started` means the time went into connecting to the sprite, not into running the command. That's Sprites API or network slowness, not `sp` or your sprite. Retry later, or check `sprite list` directly. `sp` doesn't retry uploads that die on this timeout, and retries a reattach once if it fails within 10s.

### "Warning: setup task ... failed"

The connect continues without that task. Any setup can be re-run by connecting again; `setup.conf` items can be re-run with `sp setup`.

### Claude asks you to log in, or Remote Control is rejected

Check the note `sp` printed during setup. "No working claude.ai login ... falling back to the inference-only setup-token" means the sprite has no usable full-scope login: log in to Claude Code locally so fresh credentials get pushed, or run `claude` then `/login` on the sprite. If `CLAUDE_CODE_OAUTH_TOKEN` is set in a pane, it masks the login; open a new pane after reconnecting.

`/login` inside an `sp` session opens your local browser and completes without pasting a code: `sp` watches the sprite for Claude's login URL, forwards its localhost callback port for five minutes, and opens the URL. If the Sprites API is slow, the callback page can load before the forward is up; reload it. If `sp` isn't attached (for example in `sp rc`'s headless service), you'll still be asked for the code.

### Sync isn't working

```bash
sp status <sprite-name>
sp resync .
sp daemon logs -n 50
mutagen sync list sprite-<sprite-name>
```

If the daemon exited on its idle timeout, `sp resync .` starts sync again. Stale `~/.ssh/config` entries are cleaned up when the daemon starts; `sp daemon restart` forces that.

### Terminal problems after disconnecting

`sp` resets mouse-tracking modes on exit. If raw escape sequences still appear when scrolling (for example after a crash), run:

```bash
printf '\e[?1000l\e[?1003l\e[?1006l'
```

The initial window size is set when the session starts, but resizing the local window mid-session doesn't propagate. Detach and reconnect to reflow.

### A command exits with an error

Errors are printed as `Error: ...` on stderr with exit status 1. Add `-v` for more detail on connects.

---

## License

MIT
