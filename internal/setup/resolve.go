package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// SpriteFile represents the .sprite JSON file that binds a directory to a sprite.
type SpriteFile struct {
	Organization string `json:"organization"`
	Sprite       string `json:"sprite"`
}

// ResolvedTarget contains the result of resolving a target directory
// into a sprite name and paths.
type ResolvedTarget struct {
	SpriteName string // computed sprite name
	LocalPath  string // absolute local path
	RemotePath string // remote path on sprite (e.g., /home/sprite/flyctl)
	Repo       string // GitHub owner/repo if applicable
	Org        string // Fly organization if from .sprite file
}

var (
	// Matches SSH URLs like git@github.com:owner/repo.git
	sshGitRemoteRe = regexp.MustCompile(`git@github\.com:([^/]+)/([^/]+?)(?:\.git)?$`)
	// Matches HTTPS URLs like https://github.com/owner/repo.git
	httpsGitRemoteRe = regexp.MustCompile(`https?://github\.com/([^/]+)/([^/]+?)(?:\.git)?$`)
)

// ResolvePath resolves a local directory path into a sprite target.
// Priority: .sprite file > GitHub remote > directory basename.
func ResolvePath(dir string) (*ResolvedTarget, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute path: %w", err)
	}

	basename := filepath.Base(absDir)
	result := &ResolvedTarget{
		LocalPath:  absDir,
		RemotePath: "/home/sprite/" + basename,
	}

	// Check for .sprite file first (highest priority)
	if sf, err := readSpriteFile(absDir); err == nil && sf != nil {
		result.SpriteName = sf.Sprite
		result.Org = sf.Organization
		return result, nil
	}

	// Try to detect GitHub remote
	repo, err := detectGitHubRepo(absDir)
	if err == nil && repo != "" {
		result.Repo = repo
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 {
			result.SpriteName = fmt.Sprintf("gh-%s--%s", parts[0], parts[1])
			result.RemotePath = "/home/sprite/" + parts[1]
		}
		return result, nil
	}

	// Fallback: use directory basename
	result.SpriteName = "local-" + sanitizeName(basename)
	return result, nil
}

// ResolveRepo resolves a GitHub owner/repo string into a sprite target.
func ResolveRepo(ownerRepo string) (*ResolvedTarget, error) {
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format %q: expected owner/repo", ownerRepo)
	}
	owner, repo := parts[0], parts[1]
	return &ResolvedTarget{
		SpriteName: fmt.Sprintf("gh-%s--%s", owner, repo),
		RemotePath: "/home/sprite/" + repo,
		Repo:       ownerRepo,
	}, nil
}

// readSpriteFile reads and parses a .sprite file from the given directory.
func readSpriteFile(dir string) (*SpriteFile, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".sprite"))
	if err != nil {
		return nil, err
	}
	var sf SpriteFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parsing .sprite file: %w", err)
	}
	if sf.Sprite == "" {
		return nil, nil
	}
	return &sf, nil
}

// detectGitHubRepo extracts the owner/repo from the git remote origin URL.
func detectGitHubRepo(dir string) (string, error) {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	url := strings.TrimSpace(string(out))
	return parseGitHubURL(url), nil
}

// parseGitHubURL extracts owner/repo from a GitHub remote URL.
// Returns empty string if the URL is not a GitHub URL.
func parseGitHubURL(url string) string {
	if m := sshGitRemoteRe.FindStringSubmatch(url); len(m) == 3 {
		return m[1] + "/" + m[2]
	}
	if m := httpsGitRemoteRe.FindStringSubmatch(url); len(m) == 3 {
		return m[1] + "/" + m[2]
	}
	return ""
}

// sanitizeName replaces characters that aren't safe for sprite names.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	replacer := strings.NewReplacer(
		" ", "-",
		".", "-",
		"_", "-",
		":", "-",
	)
	return replacer.Replace(name)
}
