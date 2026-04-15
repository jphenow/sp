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

// gitVolatileIgnores are .git internal files that are transient lock files or
// in-progress operation state that must NOT be synced bidirectionally. These
// files exist briefly during git operations and should never cross machines.
//
// IMPORTANT: Files like .git/index, .git/refs/*, .git/logs/, and .git/FETCH_HEAD
// MUST be synced so that commits, pushes, and branch operations on one side are
// reflected on the other. Only exclude files that are:
//   - Lock files (*.lock) — transient, cause conflicts if synced
//   - In-progress multi-step operation state (rebase, cherry-pick, bisect)
//
// These patterns are appended AFTER the "!.git" un-ignore rule so that the
// .git directory itself is still synced.
var gitVolatileIgnores = []string{
	// Lock files created transiently during git operations. These exist for
	// milliseconds and should never be synced — they cause spurious conflicts
	// and can prevent git operations on the other side.
	".git/index.lock",
	".git/refs/**/*.lock",
	".git/packed-refs.lock",
	".git/config.lock",
	".git/HEAD.lock",
	".git/shallow.lock",
	// In-progress rebase/cherry-pick/revert/bisect state. These directories
	// and files track multi-step operations that are local to one side.
	// Syncing them mid-operation can corrupt the rebase/cherry-pick state.
	".git/rebase-merge",
	".git/rebase-apply",
	".git/sequencer",
	".git/BISECT_LOG",
	".git/BISECT_NAMES",
	".git/BISECT_TERMS",
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
