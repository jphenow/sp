package setup

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jphenow/sp/internal/sprite"
)

// claudeConfigAllowlist enumerates the entries under ~/.claude/ that we
// upload to the sprite on every connect. Intentionally conservative:
//   - Preferences and global instructions (settings.json, CLAUDE.md)
//   - User-authored extensibility (commands, skills, plugins, agents,
//     keybindings) — the stuff the user presumably wants available in
//     every sprite claude session
//
// Deliberately excluded:
//   - projects/ (huge: per-project conversation history + memory)
//   - history.jsonl, sessions/, todos/, tasks/, plans/ (local state)
//   - cache/, debug/, downloads/, image-cache/, paste-cache/,
//     backups/, file-history/, session-env/, shell-snapshots/, ide/
//     (ephemeral / machine-local)
//   - statsig/, telemetry/, usage-data/, stats-cache.json (telemetry)
//
// Add to this list if a new Claude feature ships a user-facing config
// directory that should follow the user between machines.
// claudeConfigAllowlist enumerates the entries under ~/.claude/ that we
// upload to the sprite on every connect. Intentionally conservative:
//   - Preferences and global instructions (settings.json, CLAUDE.md)
//   - User-authored extensibility (commands, skills, plugins, agents,
//     keybindings, statusline) — the stuff the user presumably wants
//     available in every sprite claude session
//
// IMPORTANT: session state must NEVER be added here. The sprite builds
// its own conversation history, project memory, and session state — pushing
// the local machine's would clobber in-progress work. If you're tempted to
// add an entry, check it against claudeConfigDenylist first.
var claudeConfigAllowlist = []string{
	"CLAUDE.md",
	"settings.json",
	"settings.local.json",
	"keybindings.json",
	"statusline-command.sh",
	"commands",
	"skills",
	"plugins",
	"agents",
	"marketplace",
	"marketplaces",
}

// claudeConfigDenylist is a safety net: entries that must NEVER appear in
// the allowlist because they contain sprite-local session state that would
// be destroyed by a local→sprite push. PushClaudeConfig panics at init
// time if any of these are also in the allowlist, so a bad merge or
// refactor is caught immediately rather than silently nuking a user's
// conversation history on their next connect.
var claudeConfigDenylist = []string{
	"projects",
	"sessions",
	"history.jsonl",
	"backups",
	"file-history",
	"todos",
	"tasks",
	"plans",
	"statsig",
	"telemetry",
	"usage-data",
	"stats-cache.json",
	"cache",
	"debug",
	"downloads",
	"image-cache",
	"paste-cache",
	"session-env",
	"shell-snapshots",
	"ide",
}

// claudeConfigExclude lists paths WITHIN allowlisted entries that should
// be skipped during tar building. These are subdirectories that are large
// and/or unnecessary on the sprite — claude can fetch them itself.
// Paths are relative to ~/.claude/ and matched as prefixes.
var claudeConfigExclude = []string{
	// Full git clones of marketplace repos (~30MB). The sprite only needs
	// the plugin config JSONs + the small cache/ with installed plugin code.
	// Claude refetches marketplace repos on first plugin operation.
	"plugins/marketplaces",
	"plugins/repos",
	"plugins/data",
}

func init() {
	deny := make(map[string]bool, len(claudeConfigDenylist))
	for _, d := range claudeConfigDenylist {
		deny[d] = true
	}
	for _, a := range claudeConfigAllowlist {
		if deny[a] {
			panic("claudeConfigAllowlist contains denied entry: " + a)
		}
	}
}

// PushClaudeConfig tars the allow-listed entries under ~/.claude/ and
// extracts them into ~/.claude/ on the sprite. Called on every connect so
// local changes to preferences, skills, plugins, etc. propagate without
// needing an explicit sync. Symlinks (notably skills/ in the author's
// setup) are followed so the sprite sees real content.
//
// Returns nil if ~/.claude/ doesn't exist locally — the user just hasn't
// configured anything yet, nothing to push.
func PushClaudeConfig(client *sprite.Client, spriteName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}
	claudeDir := filepath.Join(home, ".claude")
	if _, err := os.Stat(claudeDir); err != nil {
		// No local .claude dir — nothing to push, not an error.
		return nil
	}

	// Gather the entries that actually exist. No point tarring nothing.
	var present []string
	for _, name := range claudeConfigAllowlist {
		p := filepath.Join(claudeDir, name)
		if _, err := os.Stat(p); err == nil {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return nil
	}

	tmpFile, err := os.CreateTemp("", "sp-claude-config-*.tar.gz")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	gw := gzip.NewWriter(tmpFile)
	tw := tar.NewWriter(gw)

	for _, name := range present {
		localPath := filepath.Join(claudeDir, name)
		if err := addToTarFollowingSymlinks(tw, claudeDir, localPath); err != nil {
			// Non-fatal: warn and skip. We want PushClaudeConfig to be
			// best-effort so a single broken entry (unreadable file,
			// stale symlink) doesn't block the whole connect flow.
			fmt.Fprintf(os.Stderr, "Warning: packing %s: %v\n", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Upload and extract into the sprite's ~/.claude/. mkdir -p first so a
	// fresh sprite without ~/.claude/ doesn't fail the extract. The --strip
	// is avoided because tar entries are already stored with paths relative
	// to ~/.claude/ (see addToTarFollowingSymlinks).
	extractCmd := "mkdir -p ~/.claude && cd ~/.claude && tar xzf /tmp/sp-claude-config.tar.gz && rm -f /tmp/sp-claude-config.tar.gz"
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", extractCmd},
		Files:   map[string]string{tmpFile.Name(): "/tmp/sp-claude-config.tar.gz"},
	}); err != nil {
		return fmt.Errorf("extracting claude config on sprite: %w", err)
	}
	return nil
}

// addToTarFollowingSymlinks walks a path and adds all files to the tar,
// following symlinks so their real content is included. Paths inside the
// tar are stored relative to base (which is ~/.claude/) so extraction
// into ~/.claude/ on the sprite lands files in the right place.
//
// Entries whose relative path matches any prefix in claudeConfigExclude
// are silently skipped — this is how we keep the 30MB marketplace git
// repos out of the tar while still including the small plugin config
// files alongside them.
func addToTarFollowingSymlinks(tw *tar.Writer, base, path string) error {
	info, err := os.Stat(path) // Stat follows symlinks; Lstat would not
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return fmt.Errorf("rel %s: %w", path, err)
	}

	// Check excludes — skip entire subtrees that match.
	for _, excl := range claudeConfigExclude {
		if rel == excl || strings.HasPrefix(rel, excl+"/") {
			return nil
		}
	}

	if !info.IsDir() {
		return writeTarFile(tw, path, rel, info)
	}

	// Directory entry
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("header for %s: %w", path, err)
	}
	hdr.Name = rel + "/"
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing dir header for %s: %w", rel, err)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("reading dir %s: %w", path, err)
	}
	for _, entry := range entries {
		child := filepath.Join(path, entry.Name())
		if err := addToTarFollowingSymlinks(tw, base, child); err != nil {
			return err
		}
	}
	return nil
}

// writeTarFile writes a single regular file into the tar with the given
// tar-relative name.
func writeTarFile(tw *tar.Writer, srcPath, relName string, info os.FileInfo) error {
	// Skip sockets, devices, pipes — nothing useful for config sync.
	mode := info.Mode()
	if mode&(os.ModeSocket|os.ModeDevice|os.ModeNamedPipe|os.ModeCharDevice) != 0 {
		return nil
	}

	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("header for %s: %w", srcPath, err)
	}
	// Normalize to forward slashes; tar stores POSIX paths even when
	// written on Windows (not relevant here, but defensive).
	hdr.Name = strings.ReplaceAll(relName, string(filepath.Separator), "/")
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing header for %s: %w", relName, err)
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", srcPath, err)
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copying %s: %w", srcPath, err)
	}
	return nil
}
