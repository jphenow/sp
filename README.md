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

### Organizations

Every command takes `-o`/`--org`, and reads `SP_ORG` when the flag is absent:

```sh
sp t3 setup jphenow/sp --org other-org   # one command, one organization
SP_ORG=other-org sp .                    # one shell
```

Precedence is `--org` → `SP_ORG` → the `organization` in the directory's
`.sprite` file → whatever `sprite org set-default` is set to. With none of the
first three set, `sp` passes no `-o` at all, so the sprite CLI's default stands
and nothing about everyday use changes.

That is the point of the ordering: switching organizations for one command or
one shell needs no configuration and leaves your default alone. Note that a
`.sprite` file is only read when it names a `sprite` as well — an
`organization` on its own is ignored.

A sprite belongs to exactly one organization, so the same repo in two
organizations is two sprites. If a command reports `no sprite found`, check
which organization you are pointed at before anything else.

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

## The hub — reviving sprites from a phone

A laptop losing its connection lets sprites idle-pause, and there is no good way
to bring one back from a phone. `sp hub` runs a small web app on one sprite that
lists every sprite in the org and offers the revival ladder on each: wake the
machine, start Claude, hold it Active, hand it to Remote Control.

```bash
sp hub deploy my-hub                      # build, upload and start the hub
sp hub install gh-owner-repo --hub-url …  # install the reporting agent on a sprite
```

`deploy` cross-compiles `sp` for linux/amd64 (same requirement as `--web-proxy`),
uploads it, and registers a `sp-hub` sprite-env service with an HTTP port — so an
HTTP request wakes the hub itself. The page is served at the hub sprite's own URL,
which is already behind the Sprites org login; it is private without any extra
auth. Open it on a phone and tap.

### Why there is an agent

Asking a sprite anything wakes it. Without help, the page could only show what the
org listing already knows (the machine state) and would have to wake a sprite just
to learn whether Claude is alive — waking the whole fleet on every page load.

The optional agent inverts that. `sp hub install` registers a `sp-hub-agent`
sprite-env service that reports tmux, Claude, Remote Control and hold state *while
its sprite happens to be awake*, so the page shows last-known state with an age and
costs one API call for the whole list. It also reports resumable Claude sessions,
which is what makes `--resume` usable from a phone: without it, finding out what
could be resumed costs most of what resuming would have.

A cold machine's report is ignored rather than shown — a cold restart resets the
process namespace, so a report claiming Claude was alive describes a process that
no longer exists.

The agent posts to the hub URL with a Sprites org token (`--token`, or
`$SPRITE_API_TOKEN`), which sprite-auth URLs accept alongside browser logins.

### Repairing an expired login

The `/login` browser flow redirects to `localhost`, which a phone cannot reach — so
a sprite whose Claude credentials expired is stuck when there is no laptop around.
`sp hub deploy --vault` uploads this machine's Claude credentials to the hub, which
adds a "fix auth" button that re-installs them on a sprite. It is off unless you
pass the flag, because it means your credentials live on that sprite.

### Holds

The hub takes the hold, not the page. A phone browser is not a reliable heartbeat —
iOS suspends background tabs, which would drop the hold exactly when you put the
phone down mid-task. Holds use the same Tasks API heartbeat as `sp keepalive` and
self-release at the deadline (`--hold-for`, default 1h).

---

## T3 Code on a sprite

[T3 Code](https://github.com/pingdotgg/t3code) drives coding agents from a phone,
web or desktop app. `sp t3 setup` joins a sprite to it and leaves a service
behind, so a sprite that goes cold rejoins on its own when it is woken.

```bash
sp t3 setup jphenow/sp                  # over T3 Connect, T3's hosted relay
sp t3 setup jphenow/sp --tailscale      # over your own tailnet instead
sp t3 setup jphenow/sp --for 4h         # hold the sprite Active longer
sp t3 pair jphenow/sp --label desktop   # a code for each client you connect
sp t3 pair jphenow/sp --label phone
```

Targets are the same ones `sp` itself takes: `.`, `owner/repo`, or a sprite name.

Both paths reach the sprite over a connection the sprite dials *out*, so neither
needs port forwarding and neither exposes anything on the sprite's own URL.
Neither can wake a sleeping sprite either — that takes a relay; see [Waking a
sprite from a phone](#waking-a-sprite-from-a-phone).

**T3 Connect** is the default. Setup is interactive once per sprite: T3's device
flow prints a URL and a code to approve in a browser. In exchange you get push
notifications and Live Activities on the phone, which are a function of the
relay. Note that the relay serves an unauthenticated descriptor naming the
environment and its OS; the API behind it requires authorization.

**`--tailscale`** uses your tailnet instead. Nothing is registered with a T3
account and no third party sits in the middle. T3 is served over Tailscale Serve
on an HTTPS name only your own devices can resolve, and you pair by scanning a
QR code. You give up the push notifications.

### Putting sprites on your tailnet

```bash
sp tailscale setup jphenow/sp                   # join one sprite
sp tailscale setup jphenow/sp --tags tag:dev    # use a tag you already own
sp tailscale setup jphenow/sp --tags none       # don't advertise any tag
```

This needs a reusable auth key. Create one at **Settings → Keys → Generate auth
key** in the [Tailscale admin console](https://login.tailscale.com/admin/settings/keys):

- **Reusable: on** — every sprite joins with the same key.
- **Ephemeral: off** — ephemeral nodes are *deleted* when they go offline, and a
  sprite that sleeps would vanish from the tailnet.
- **Pre-approved: on**, if the option appears. It only does when your tailnet
  requires device approval, and nothing on a sprite can answer an approval
  prompt.
- **Tags: leave empty** for now; see below.

Then:

```bash
mkdir -p ~/.config/sprite
printf '%s' 'tskey-auth-…' > ~/.config/sprite/tailscale.authkey
chmod 600 ~/.config/sprite/tailscale.authkey
printf 'none' > ~/.config/sprite/tailscale.tags
```

Once that key exists, **every connect joins its sprite to the tailnet** as one
more setup task. It is idempotent and short-circuits on a single call for a
sprite that is already on, and it is non-fatal: a sprite that could not reach the
tailnet is still a sprite you can work in. No key, no task, no round trips —
which is how you opt out.

The key is read from `TS_AUTHKEY` or `~/.config/sprite/tailscale.authkey`; the
tags from `TS_TAGS` or `~/.config/sprite/tailscale.tags`, defaulting to
`tag:sprite`, with `none` meaning untagged.

### Owning a tag

Tags default to `tag:sprite`, which your tailnet's policy file has to own —
and the key-creation page will not offer you a tag that does not exist there
yet. That is why the quickstart above says `none`.

It is worth doing eventually: an untagged node's key expires (180 days by
default) and the node then drops off the tailnet until someone logs in again,
which is the one thing a sprite with no browser cannot do. Tagged nodes do not
expire.

In [Access controls](https://login.tailscale.com/admin/acls/file), add
`tag:sprite` to `tagOwners` — a line inside the existing block if there is one,
or the whole block at the top level if not:

```jsonc
"tagOwners": {
  "tag:sprite": ["autogroup:admin"],
},
```

If your `acls` is still the default allow-all, that is enough. If you have
locked it down, also let your own devices reach the tag:

```jsonc
{"action": "accept", "src": ["autogroup:member"], "dst": ["tag:sprite:*"]}
```

Save, regenerate the auth key with `tag:sprite` selected, update
`~/.config/sprite/tailscale.authkey`, and `rm ~/.config/sprite/tailscale.tags`
to go back to the default. New sprites pick it up. To move an existing sprite
onto the tag, delete that machine in the admin console and re-run `sp tailscale
setup` — its tailscaled reports `NeedsLogin` and rejoins, tagged.

### How it runs on the sprite

`tailscaled` is a `--restart always` sprite service, so it comes back after a
cold wake, and its state lives on the sprite's own volume, so the node keeps its
address instead of appearing as a new machine every time.

Three details are load-bearing:

- **`--tun=userspace-networking`.** A sprite is a container. The guest's device
  allowlist does know `/dev/net/tun`, but only grants it to a sprite whose
  privileges policy asks for it. Userspace mode needs no device and no
  `CAP_NET_ADMIN`, and still terminates inbound connections — all this needs.
  The cost is that other processes on the sprite cannot route *out* over the
  tailnet.
- **`--accept-dns=false`.** The guest runtime manages `/etc/resolv.conf`.
  Letting tailscaled take DNS over would fight it, and the loser would be every
  name the sprite already resolves.
- **The auth key reaches the sprite as a `chmod 600` file** and `tailscale up`
  is given the path, so the key is never readable in the sprite's process list.

One trap: the guest's default egress allowlist contains **no** Tailscale or DERP
domains. Filtering is off unless a network policy exists, so ordinary sprites are
fine — but applying a network policy to a sprite will silently cut its tailnet
connection.

### Waking a sprite from a phone

Tailscale is outbound-only, exactly like T3's relay and like Remote Control: a
sleeping sprite answers on your tailnet no more than it answered on the relay.
Waking needs an inbound request through Fly's edge to the sprite's own URL, and
the edge wants an org login that a phone app cannot complete.

`sp relay serve` closes that gap. Run it on a machine that stays awake and is on
the same tailnet as your phone. It gives each sprite a port, holds a Sprites
credential, and forwards to the sprite's `sp-t3` service through the edge — so
connecting is the wake. No login on the phone, nothing public, no manual step.

```bash
# On the always-on machine
printf '8899 gh-owner-repo\n8900 gh-owner-other\n' > ~/.config/sp/relay.list
SPRITE_RELAY_TOKEN=… sp relay serve --org my-org --bind "$(tailscale ip -4)"

# On your laptop, so sp t3 pair hands out relay addresses
printf 'relay-host.your-tailnet.ts.net' > ~/.config/sp/relay.host
cp relay.list ~/.config/sp/relay.list
```

Then `sp t3 pair <sprite> --label phone` prints a code and the relay address to
enter in the T3 app.

Three things matter:

- **The credential should be narrow.** Mint a child token with `POST /v1/tokens`
  and `privileges: ["service.proxy"]`, `services: {"sp-t3": []}`, and `sprites`
  either named or `{"*": ["proxy"]}`. It reaches T3 and nothing else: no exec,
  no reading or destroying sprites, no other services.
- **The organization needs `target_header_routing`.** The relay sends the
  credential as `X-Sprite-Authorization`, because T3 sends its own session
  token as `Authorization` and overwriting it leaves T3 with no credential.
  Without the feature the edge ignores the separate header and answers 302.
- **`--bind` is required.** The relay lends its credential to anything that can
  reach it, so it should listen on the tailnet address and nowhere else.

The first request after a sprite sleeps is held while the sprite wakes, usually
a few seconds, rather than failing. `sp bridge` does a similar job over the
sprite CLI's port proxy, which the Sprites 2 runtime does not have.

### Staying awake while the agent works

A sprite stops 30 seconds after its last exec, shell or open connection, and
running services do not count. An agent left working with the phone in a
pocket has half a minute to live.

`sp t3 setup --tailscale` therefore installs an `sp-awake` service that holds
the sprite awake while T3 is working and lets it sleep once it stops. Working
means either a run T3 has not finished (the model is thinking) or a provider
process running a command, including one sent to the background — which is what
an agent does with anything long, and which T3 records as finished the moment it
is launched. T3's provider process merely existing does not count: T3 keeps it
alive between turns.

Anything else the provider keeps running as a child, such as an MCP server,
reads as a command and holds the sprite awake. That errs toward cost rather than
toward losing work.

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
| `sp hub deploy <sprite>` | Build and start the hub web app on a sprite (`--vault`) |
| `sp hub install <sprite>` | Install the reporting agent (`--hub-url`, `--token`) |
| `sp hub serve` | The hub web app; runs on the hub sprite |
| `sp hub agent` | The reporting agent; runs on a watched sprite |
| `sp t3 setup <target>` | Join a sprite to T3 Code (`--tailscale`, `--for`) |
| `sp t3 pair <target>` | Print a new pairing QR code (tailnet mode) |
| `sp tailscale setup <target>` | Install tailscale and join the sprite to your tailnet (`--tags`) |
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
| `-o`, `--org ORG` | Sprites organization; see [Organizations](#organizations). Available on every command, not just connect. |
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

### A sprite isn't reachable on the tailnet

Connect only warns when the join fails, because losing the session to it would be
worse. Run the join on its own to see the actual error:

```bash
sp tailscale setup <target>
sprite exec <sprite> -- /usr/local/bin/tailscale status
```

- **`sudo: a password is required`** — installing the binaries and running the
  daemon need passwordless sudo on the sprite.
- **`requested tags [tag:…] are invalid or not permitted`** — your tailnet's
  policy file does not own that tag, or you do not. See
  [Owning a tag](#owning-a-tag), or pass `--tags none`.
- **The name does not resolve on your phone** — the phone has to be on the
  tailnet itself, with the Tailscale app connected and MagicDNS on.
- **It worked and then stopped** — check whether the sprite paused. A hold is
  what keeps it up; the tailnet cannot wake it.

### A command exits with an error

Errors are printed as `Error: ...` on stderr with exit status 1. Add `-v` for more detail on connects.

---

## License

MIT
