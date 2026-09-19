# Plan: Make `sp` Monorepo-Aware

## Problem

When `sp .` runs from a monorepo subdirectory (e.g., `~/monorepo/packages/api`):
1. **Naming collision**: Detects the GitHub remote and names the sprite `gh-owner--repo` — same name for every subdirectory in the repo
2. **Path mismatch bug**: Sets `target_dir_name` to the repo basename (`monorepo`), but the tar archive creates a directory named after the subdirectory (`api/`). Claude tries to open at `/home/sprite/monorepo` which doesn't exist
3. **No full-repo option**: Sometimes you want the whole repo context, not just one service

## Solution

Three changes to `sp`:
1. **Subpath-aware sprite naming** — detect git root, compute relative path, encode it in sprite name
2. **`--full` flag** — opt into syncing the entire repo from git root
3. **Colon syntax for `owner/repo`** — `sp owner/repo:packages/api` clones full repo, opens at subpath

## File: `sp`

### 1. New global variable (~line 27)

```bash
FULL_MODE=false
```

### 2. New helper: `get_git_root()` (after line 490)

Returns the physical path of the git repo root, or empty string if not in a git repo. Uses `pwd -P` to resolve symlinks consistently.

```bash
get_git_root() {
    local root
    root=$(git rev-parse --show-toplevel 2>/dev/null) || return 0
    (cd "$root" && pwd -P)
}
```

### 3. New helper: `sanitize_subpath()` (after `dir_to_sprite_name`)

Converts a relative path like `packages/api` into a sprite-name-safe string: `packages-api`. Replaces `/` with `-`, strips special chars, collapses double dashes.

```bash
sanitize_subpath() {
    local subpath="$1"
    echo "$subpath" | tr '/' '-' | sed 's/[^a-zA-Z0-9-]/-/g' | sed 's/--*/-/g' | sed 's/^-//;s/-$//'
}
```

### 4. Modify `repo_to_sprite_name()` (line 493)

Add optional second `subpath` argument. When provided, appends `--<sanitized-subpath>` to the name.

- `repo_to_sprite_name "owner/repo"` -> `gh-owner--repo` (unchanged)
- `repo_to_sprite_name "owner/repo" "packages/api"` -> `gh-owner--repo--packages-api`

Same change to `dir_to_sprite_name()` (line 499).

### 5. Rewrite `handle_current_dir_mode()` (line 770) -- core change

New logic flow:
1. Use `pwd -P` for consistent path resolution
2. Call `get_git_root()` and compute `rel_path` (relative path from git root)
3. Branch on three cases:

| Case | `source_dir` | `sprite_name` | Claude opens at |
|------|-------------|---------------|----------------|
| `--full` | git root | `gh-owner--repo` | `/home/sprite/<repo>/` |
| In git subdir | current dir | `gh-owner--repo--pkg-api` | `/home/sprite/<subdir>/` |
| At root / no git | current dir | `gh-owner--repo` or `local-<name>` | `/home/sprite/<dir>/` |

**Fixes the existing path mismatch bug**: `target_dir_name` is always set to `basename(source_dir)`, matching what the tar actually creates.

### 6. Modify `handle_repo_mode()` (line 680)

Accept optional `subpath` argument. When provided:
- Clone the full repo (unchanged)
- Validate the subpath exists in the cloned repo
- Open Claude at `/home/sprite/<repo>/<subpath>` instead of the repo root
- Use subpath-aware sprite name

### 7. Modify `main()` argument parsing (line 867)

- Parse `--full` flag (only meaningful with `sp .`, warn + ignore otherwise)
- Parse colon syntax: `owner/repo:path/to/pkg` splits into repo + subpath

### 8. Update usage header (lines 5-16)

Document new syntax and flags.

## Naming examples

| Command | Sprite name |
|---------|-------------|
| `sp .` from repo root | `gh-owner--repo` |
| `sp .` from `packages/api` | `gh-owner--repo--packages-api` |
| `sp .` from `services/web` | `gh-owner--repo--services-web` |
| `sp . --full` from any subdir | `gh-owner--repo` |
| `sp owner/repo` | `gh-owner--repo` |
| `sp owner/repo:packages/api` | `gh-owner--repo--packages-api` |

## Edge cases

- **Non-git directory**: No monorepo logic fires, `local-<dirname>` naming unchanged
- **At git root**: `rel_path` is empty, behavior identical to current
- **`--full` with no git**: Warning printed, flag ignored, uses current dir
- **`--full` + `--sync`**: Works -- syncs entire repo bidirectionally (larger scope)
- **Two subdirs with same basename** (e.g., `packages/api` and `services/api`): Different sprite names due to full relative path in name
- **`owner/repo:nonexistent`**: Validated after clone, errors with clear message

## Documentation updates

- `README.md`: Add monorepo section with usage examples and naming table
- `CONTRIBUTING.md`: Add monorepo test scenarios
- `AGENTS.md`: Add monorepo workflow patterns

## Verification

1. Test `sp .` from a monorepo subdirectory -- should create a subpath-specific sprite
2. Test `sp .` from the repo root -- behavior unchanged
3. Test `sp . --full` from a subdirectory -- should sync full repo
4. Test `sp owner/repo:path` -- should clone full repo, open at subpath
5. Test `sp .` from a non-git directory -- should work as before (`local-<dirname>`)
6. Test `sp . --sync` from a subdirectory -- bidirectional sync should work with the subpath sprite
7. Verify two different subdirectories produce different sprite names (no collision)
