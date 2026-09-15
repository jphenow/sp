# Contributing to sp

`sp` is a Go program. This covers building it, how the code is laid out, and what to check before opening a pull request. For what the tool does, see [README.md](README.md).

## Getting started

```bash
git clone https://github.com/YOUR_USERNAME/sp.git
cd sp
git checkout -b feature/your-feature-name
make build        # -> ./sp-bin
```

You need the Go version in `go.mod` (1.25.5), plus the `sprite` CLI, Mutagen and an `~/.ssh/id_ed25519` key to exercise anything end to end.

## Make targets

| Target | What it does |
|--------|--------------|
| `make build` | `go build -o sp-bin .` |
| `make install` | `go install .` |
| `make test` | `go test ./... -v` |
| `make test-race` | `go test ./... -race -v` |
| `make lint` | `golangci-lint run ./...` (install golangci-lint separately) |
| `make tidy` | `go mod tidy` |
| `make clean` | Remove `sp-bin` and `sp-linux-amd64` |
| `make all` | `tidy`, `build`, `install` |

## Layout

| Path | Contents |
|------|----------|
| `main.go` | Calls `cmd.Execute()` and prints any returned error |
| `cmd/` | Cobra commands, one file per command. `root.go` maps `sp <target>` to `connect`; `connect.go` is the connect flow and session handling; `keepalive.go` has the Tasks API heartbeat used by holds, `--keep-warm`, `keepalive` and `rc`. |
| `internal/setup/` | Target resolution (`resolve.go`), credential and config pushes (`auth.go`, `claude_config.go`), `setup.conf` parsing and execution (`config.go`), batched uploads with retry (`upload.go`) |
| `internal/sprite/` | Wrapper around the `sprite` CLI, plus call tracing (`trace.go`) and debug-log stall analysis (`dialgap.go`) |
| `internal/progress/` | The multi-task progress display used during connect |
| `internal/daemon/` | Background daemon (Unix socket JSON RPC), its client, and the sync/proxy health monitor |
| `internal/sync/` | Mutagen sessions, the `sprite proxy` SSH tunnel, `~/.ssh/config` entries, `.gitignore` conversion |
| `internal/store/` | SQLite database at `~/.config/sp/sp.db` |
| `internal/tui/` | Bubble Tea dashboard |
| `internal/serve/` | Reverse proxy run on the sprite by `sp serve` (`--web-proxy`) |
| `internal/logging/` | Daemon logging to `~/.config/sp/sp.log` |

## Developing

Run your build directly: `./sp-bin .`, `./sp-bin tui`, and so on.

Keep in mind that the daemon is shared. Whichever binary starts it first owns it, and later commands from any `sp` binary talk to that process over `~/.config/sp/sp.sock`. The daemon and the TUI re-exec themselves when their own binary changes on disk, so rebuilding the binary the daemon was started from is enough; if it was started from a different binary (for example the installed `sp`), run `sp daemon restart` from that binary or kill it and let your build start a new one. Daemon output goes to `~/.config/sp/sp.log` (`sp daemon logs -f`).

A connect's cost is dominated by the number of `sprite` CLI calls, each of which pays a connection to the sprite. When adding setup work, fold it into an existing call (see `UploadFilesRunning` and `HomePermissionsScript`) rather than adding a new one. `-v` prints the per-call timing profile.

Never print or trace command environments or scripts that embed credentials; `describeExec` in `internal/sprite/trace.go` redacts token-shaped strings and excludes `Env` for this reason.

## Before submitting

```bash
go build ./... && go vet ./... && go test ./...
```

Unit tests don't touch a real sprite. For changes to the connect flow, sync, or the daemon, also test against a real sprite and say what you tried in the PR. Useful cases:

- **New sprite and existing sprite**: `./sp-bin .` in a GitHub checkout and in a non-git directory; `./sp-bin owner/repo`. Reconnect to confirm it reattaches to the running tmux session.
- **Claude auth**: with local Claude Code credentials, the sprite shouldn't get `CLAUDE_CODE_OAUTH_TOKEN` (`echo $CLAUDE_CODE_OAUTH_TOKEN` in a new pane is empty and `claude auth status` reports `claude.ai`). With only `~/.claude-token`, `sp` prints the setup-token note and Claude still works.
- **Remote Control**: `./sp-bin . --rc` on a fresh sprite, then join from the Claude app.
- **setup.conf**: `sp conf init`; `[files]` with `[newest]`, `[always]` and `src -> dest`; `[commands]` with and without `!`; `sp setup .`.
- **Sync**: edits in both directions; a new `.gitignore` rule picked up by `sp resync .`; `.git` state (commit on the sprite, see it locally); a conflict shows in `sp status <name>` and the TUI; sync stops when the sprite pauses and resumes when it wakes.
- **Variants and cleanup**: `./sp-bin . test-variant`, `sp status --variants`, `sp pin`/`sp unpin`, `sp prune` (dry run), `sp rm`.
- **Holds**: after exiting a session (killing the tmux session), the sprite is released within a minute or so; `--no-hold`; `sp keepalive . --stop`.
- **Failure paths**: a missing SSH key, no Claude credentials at all (prompt), an invalid `owner/repo`, and a slow sprite (the stall notes and timing profile should make the cause clear).

## Pull requests

- Keep each PR focused, and describe what problem it solves and how you tested it.
- Update README.md (and AGENTS.md if it affects Claude on the sprite) when you change commands, flags or behavior they describe.
- Write clear commit messages that say what changed and why.

## License

By contributing to `sp`, you agree that your contributions will be licensed under the MIT License.
