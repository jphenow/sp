# Using Claude Code with sp

How to run Claude Code inside a sprite with `sp`. For installation, the full command reference, and troubleshooting, see [README.md](README.md).

## Starting Claude

```bash
sp connect . --exec claude          # Claude as the session command
sp connect owner/repo --exec claude # same, for a repo cloned on the sprite
sp . --rc                           # claude --remote-control, joinable from the Claude app
sp .                                # bash; run claude yourself in a tmux pane
```

Use `--exec` with `sp connect`. The shorthand `sp . -- claude` doesn't work: `claude` is taken as a variant name and a new sprite is created.

Each sprite has one persistent tmux session. Reconnecting reattaches to it, and Claude keeps running across disconnects, so the command you pass only matters the first time. To run another Claude alongside, open a tmux window or pane; to run one against an isolated copy of the code, use a variant (below).

## What Claude gets on the sprite

- **Auth.** Your local full-scope Claude Code login (macOS Keychain or `~/.claude/.credentials.json`) is pushed if it's fresher than the sprite's copy. `sp` checks `claude auth status` on the sprite and only falls back to the setup token from `~/.claude-token` when there's no working `claude.ai` login. See [Authentication](README.md#authentication) for why the token is otherwise kept out of the environment.
- **Permissions bypassed.** The sprite's `settings.json` gets `permissions.defaultMode: "bypassPermissions"` with the consent prompt pre-accepted, and `claude` is aliased to `claude --dangerously-skip-permissions` in the shell rc files. Sprites are disposable; treat them that way.
- **Your config.** `CLAUDE.md`, `settings.json`, `settings.local.json`, `keybindings.json`, `statusline-command.sh`, and the `commands`, `skills`, `plugins`, `agents` and `marketplace(s)` directories from `~/.claude/` are pushed on every connect. Local home-directory paths in `settings.json`'s status line and the plugin index files are rewritten to `/home/sprite`. Conversation history, project memory and other session state stay on the sprite and are never overwritten.
- **Competing auth removed.** `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and the Bedrock/Vertex/Foundry switches are unset in the shell and tmux environment.
- **Git and GitHub.** Your git name and email; `GH_TOKEN` from `gh auth token` in the environment; SSH-style GitHub URLs rewritten to HTTPS with a credential helper that reads `GH_TOKEN`. Claude can commit, push and use `gh` without passphrase prompts.
- **Your files.** For a directory target, edits Claude makes on the sprite sync back to your local checkout (and yours to the sprite), including `.git`, so branches and commits show up on both sides. Conflicting edits to the same file are held as conflicts rather than overwritten.

## Remote Control

Remote Control lets you drive a session from the Claude app or claude.ai/code. It requires a full `claude.ai` login on the sprite; the setup token is rejected.

- `sp . --rc` starts the session with `claude --remote-control`, with the Remote Control session name prefixed by the sprite name.
- In an existing session, run `/remote-control` from inside Claude.
- `sp rc .` runs Claude Remote Control headless as a sprite-env service that restarts after a cold wake and resumes the last session. Stop it with `sp rc . --stop`. Experimental.

If `sp` prints "falling back to the inference-only setup-token", Remote Control won't work until the sprite has a real login: log in to Claude Code locally and reconnect, or run `/login` on the sprite. Completing `/login` inside tmux currently needs the code pasted back manually; this is being worked on.

## Keeping the sprite reachable

By default `sp` holds the sprite Active for as long as its tmux session exists (up to 8 hours), so a long-running Claude task or a Remote Control session isn't interrupted by an idle pause when you walk away from the terminal.

```bash
sp . --no-hold                 # let it pause when idle
sp . --keep-warm 1h            # hold up to 1h, release once the pane is idle ~60s
sp keepalive . --for 3h        # hold for a fixed window, no session needed
sp keepalive . --stop
```

## Parallel experiments with variants

A variant is a separate sprite for the same repo, useful for letting Claude try an approach without touching your main environment:

```bash
sp connect . try-sqlite --exec claude
sp connect owner/repo try-sqlite --exec claude
```

A directory variant starts from a one-time upload of the files `git ls-files` reports (tracked plus untracked, minus ignored; no `.git`) and is not synced afterwards. For work you want to commit and push from the sprite, use an `owner/repo` variant, which is a full clone. Keep one with `sp pin owner/repo:try-sqlite`; clean up the rest with `sp prune` (dry run) and `sp prune --yes`, or `sp rm owner/repo:try-sqlite`.

## Tools for Claude on the sprite

Install anything Claude needs with `~/.config/sprite/setup.conf` so every new sprite has it:

```ini
[commands]
command -v npm :: npm install -g prettier
```

New sprites run the whole file on first connect; run `sp setup .` to apply it to an existing one. See [setup.conf](README.md#setupconf).

## Resources

- [README.md](README.md)
- [Sprites documentation](https://sprites.dev)
- [Claude Code documentation](https://docs.anthropic.com/en/docs/claude-code)
