package sync

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// defaultIgnores are always excluded from sync regardless of .gitignore.
var defaultIgnores = []string{
	"node_modules",
	".next",
	"dist",
	"build",
	".DS_Store",
	"._*",
}

// gitVolatileIgnores are .git internal files that change frequently during
// normal git operations (staging, committing, fetching, rebasing) and must
// NOT be synced bidirectionally. Syncing these causes:
//   - Staged changes to "vanish" (.git/index stomped by the other side)
//   - Lock file collisions (.git/index.lock, .git/refs/*.lock)
//   - Stale merge/rebase state crossing between sides
//
// These patterns are appended AFTER the "!.git" un-ignore rule so that the
// .git directory itself (objects, config, hooks, refs, HEAD) is still synced.
var gitVolatileIgnores = []string{
	// Staging area — binary file rewritten on every git add/checkout/reset
	".git/index",
	// Lock files created transiently during git operations
	".git/index.lock",
	".git/refs/**/*.lock",
	".git/packed-refs.lock",
	".git/config.lock",
	".git/HEAD.lock",
	".git/shallow.lock",
	// Transient state files from fetch/merge/rebase/cherry-pick
	".git/FETCH_HEAD",
	".git/ORIG_HEAD",
	".git/MERGE_HEAD",
	".git/MERGE_MSG",
	".git/MERGE_MODE",
	".git/CHERRY_PICK_HEAD",
	".git/REVERT_HEAD",
	".git/BISECT_LOG",
	".git/SQUASH_MSG",
	".git/COMMIT_EDITMSG",
	".git/rebase-merge",
	".git/rebase-apply",
	".git/sequencer",
	// Reflogs — each side appends entries independently on every commit,
	// checkout, rebase, fetch, etc. Bidirectional sync causes constant
	// conflicts because both sides diverge after any local operation.
	".git/logs",
	// Remote-tracking refs — each side runs git fetch independently, so
	// refs/remotes/origin/* always diverge. Only local branch refs and
	// tags need syncing.
	".git/refs/remotes",
	// Auto-stash file used during rebase
	".git/refs/stash",
	// Editor backup / git gc temp files
	".git/gc.log",
	".git/gc.log.lock",
}

// CollectIgnorePatterns walks a directory tree collecting .gitignore patterns
// and converts them into Mutagen-compatible ignore rules.
// Returns a list of patterns suitable for Mutagen's --ignore flag.
func CollectIgnorePatterns(rootDir string) []string {
	patterns := make([]string, len(defaultIgnores))
	copy(patterns, defaultIgnores)

	// Walk the directory tree looking for .gitignore files
	filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}

		// Skip directories we know to ignore
		if info.IsDir() {
			name := info.Name()
			for _, ignored := range defaultIgnores {
				if name == ignored {
					return filepath.SkipDir
				}
			}
			return nil
		}

		if info.Name() != ".gitignore" {
			return nil
		}

		// Calculate relative prefix for nested .gitignore files
		relDir, err := filepath.Rel(rootDir, filepath.Dir(path))
		if err != nil {
			return nil
		}

		gitignorePatterns := parseGitignoreFile(path, relDir)
		patterns = append(patterns, gitignorePatterns...)
		return nil
	})

	// Ensure .git is NOT ignored (Mutagen needs it for branch/object sync)
	patterns = append(patterns, "!.git")

	// Exclude volatile .git internals that cause staging area drift,
	// lock file collisions, and transient merge/rebase state conflicts.
	// These MUST come after "!.git" so the directory is synced but these
	// specific files within it are excluded.
	patterns = append(patterns, gitVolatileIgnores...)

	return deduplicatePatterns(patterns)
}

// parseGitignoreFile reads a .gitignore file and returns Mutagen-compatible patterns.
// The relDir prefix is prepended to patterns from nested .gitignore files.
func parseGitignoreFile(path, relDir string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		pattern := convertGitignorePattern(line, relDir)
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}

	return patterns
}

// convertGitignorePattern converts a single .gitignore pattern to a Mutagen
// ignore pattern, applying the relative directory prefix for nested files.
func convertGitignorePattern(pattern, relDir string) string {
	isNegation := false

	// Handle negation patterns
	if strings.HasPrefix(pattern, "!") {
		isNegation = true
		pattern = strings.TrimPrefix(pattern, "!")
	}

	// Strip leading and trailing slashes
	pattern = strings.TrimPrefix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")

	if pattern == "" {
		return ""
	}

	// Prefix with relative directory for nested .gitignore files
	if relDir != "" && relDir != "." {
		pattern = relDir + "/" + pattern
	}

	if isNegation {
		return "!" + pattern
	}
	return pattern
}

// deduplicatePatterns removes duplicate patterns while preserving order.
func deduplicatePatterns(patterns []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}
