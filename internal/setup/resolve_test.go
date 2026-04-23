package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git@github.com:superfly/flyctl.git", "superfly/flyctl"},
		{"git@github.com:superfly/flyctl", "superfly/flyctl"},
		{"https://github.com/superfly/flyctl.git", "superfly/flyctl"},
		{"https://github.com/superfly/flyctl", "superfly/flyctl"},
		{"http://github.com/jphenow/gameservers.git", "jphenow/gameservers"},
		{"git@gitlab.com:foo/bar.git", ""},
		{"https://gitlab.com/foo/bar", ""},
		{"", ""},
		{"not-a-url", ""},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := parseGitHubURL(tt.url)
			if got != tt.want {
				t.Errorf("parseGitHubURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"MyProject", "myproject"},
		{"my project", "my-project"},
		{"my.project", "my-project"},
		{"my_project", "my-project"},
		{"my:project", "my-project"},
		{"simple", "simple"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveRepo(t *testing.T) {
	result, err := ResolveRepo("superfly/flyctl", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SpriteName != "gh-superfly--flyctl" {
		t.Errorf("sprite name = %q, want %q", result.SpriteName, "gh-superfly--flyctl")
	}
	if result.BaseName != "gh-superfly--flyctl" {
		t.Errorf("base name = %q, want %q", result.BaseName, "gh-superfly--flyctl")
	}
	if result.Variant != "" {
		t.Errorf("variant should be empty, got %q", result.Variant)
	}
	if result.RemotePath != "/home/sprite/flyctl" {
		t.Errorf("remote path = %q, want %q", result.RemotePath, "/home/sprite/flyctl")
	}
	if result.Repo != "superfly/flyctl" {
		t.Errorf("repo = %q, want %q", result.Repo, "superfly/flyctl")
	}
}

func TestResolveRepoWithVariant(t *testing.T) {
	result, err := ResolveRepo("superfly/flyctl", "Auth.Ideas")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SpriteName != "gh-superfly--flyctl--auth-ideas" {
		t.Errorf("sprite name = %q, want %q", result.SpriteName, "gh-superfly--flyctl--auth-ideas")
	}
	if result.BaseName != "gh-superfly--flyctl" {
		t.Errorf("base name = %q, want %q", result.BaseName, "gh-superfly--flyctl")
	}
	if result.Variant != "auth-ideas" {
		t.Errorf("variant = %q, want %q", result.Variant, "auth-ideas")
	}
	if result.RemotePath != "/home/sprite/flyctl" {
		t.Errorf("remote path should be unchanged by variant, got %q", result.RemotePath)
	}
}

func TestResolveRepoInvalid(t *testing.T) {
	_, err := ResolveRepo("noslash", "")
	if err == nil {
		t.Error("expected error for invalid repo format")
	}
}

func TestResolvePathWithSpriteFile(t *testing.T) {
	dir := t.TempDir()
	spriteContent := `{"organization":"test-org","sprite":"my-custom-sprite"}`
	if err := os.WriteFile(filepath.Join(dir, ".sprite"), []byte(spriteContent), 0o644); err != nil {
		t.Fatalf("writing .sprite file: %v", err)
	}

	result, err := ResolvePath(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SpriteName != "my-custom-sprite" {
		t.Errorf("sprite name = %q, want %q", result.SpriteName, "my-custom-sprite")
	}
	if result.Org != "test-org" {
		t.Errorf("org = %q, want %q", result.Org, "test-org")
	}
}

func TestResolvePathWithSpriteFileAndVariant(t *testing.T) {
	dir := t.TempDir()
	spriteContent := `{"organization":"test-org","sprite":"my-custom-sprite"}`
	if err := os.WriteFile(filepath.Join(dir, ".sprite"), []byte(spriteContent), 0o644); err != nil {
		t.Fatalf("writing .sprite file: %v", err)
	}

	result, err := ResolvePath(dir, "scratch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SpriteName != "my-custom-sprite--scratch" {
		t.Errorf("sprite name = %q, want %q", result.SpriteName, "my-custom-sprite--scratch")
	}
	if result.BaseName != "my-custom-sprite" {
		t.Errorf("base name = %q, want %q", result.BaseName, "my-custom-sprite")
	}
	if result.Variant != "scratch" {
		t.Errorf("variant = %q, want %q", result.Variant, "scratch")
	}
}

func TestResolvePathFallbackToBasename(t *testing.T) {
	// Create a directory that is NOT a git repo and has no .sprite file
	dir := t.TempDir()
	subdir := filepath.Join(dir, "my-project")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	result, err := ResolvePath(subdir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SpriteName != "local-my-project" {
		t.Errorf("sprite name = %q, want %q", result.SpriteName, "local-my-project")
	}
	if result.RemotePath != "/home/sprite/my-project" {
		t.Errorf("remote path = %q, want %q", result.RemotePath, "/home/sprite/my-project")
	}
}

func TestResolvePathFallbackWithVariant(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "my-project")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	result, err := ResolvePath(subdir, "try-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SpriteName != "local-my-project--try-1" {
		t.Errorf("sprite name = %q, want %q", result.SpriteName, "local-my-project--try-1")
	}
	if result.BaseName != "local-my-project" {
		t.Errorf("base name = %q, want %q", result.BaseName, "local-my-project")
	}
}
