package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConvertGitignorePattern(t *testing.T) {
	tests := []struct {
		pattern string
		relDir  string
		want    string
	}{
		{"node_modules", ".", "node_modules"},
		{"/dist", ".", "dist"},
		{"dist/", ".", "dist"},
		{"*.log", ".", "*.log"},
		{"!important.log", ".", "!important.log"},
		{"build", "src", "src/build"},
		{"*.o", "lib/native", "lib/native/*.o"},
		{"!keep", "src", "!src/keep"},
		{"", ".", ""},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_in_"+tt.relDir, func(t *testing.T) {
			got := convertGitignorePattern(tt.pattern, tt.relDir)
			if got != tt.want {
				t.Errorf("convertGitignorePattern(%q, %q) = %q, want %q",
					tt.pattern, tt.relDir, got, tt.want)
			}
		})
	}
}

func TestCollectIgnorePatterns(t *testing.T) {
	dir := t.TempDir()

	// Create root .gitignore
	rootGitignore := "*.log\n/tmp\nbuild/\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(rootGitignore), 0o644); err != nil {
		t.Fatalf("writing root .gitignore: %v", err)
	}

	// Create nested .gitignore
	subDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}
	subGitignore := "*.generated.go\n!important.go\n"
	if err := os.WriteFile(filepath.Join(subDir, ".gitignore"), []byte(subGitignore), 0o644); err != nil {
		t.Fatalf("writing sub .gitignore: %v", err)
	}

	patterns := CollectIgnorePatterns(dir)

	// Check that default ignores are present
	assertContains(t, patterns, "node_modules")
	assertContains(t, patterns, ".DS_Store")

	// Check that root .gitignore patterns are present
	assertContains(t, patterns, "*.log")
	assertContains(t, patterns, "tmp")
	assertContains(t, patterns, "build")

	// Check that nested patterns have prefix
	assertContains(t, patterns, "src/*.generated.go")
	assertContains(t, patterns, "!src/important.go")

	// .git must NOT be ignored
	assertContains(t, patterns, "!.git")
}

func TestDeduplicatePatterns(t *testing.T) {
	input := []string{"a", "b", "a", "c", "b", "d"}
	got := deduplicatePatterns(input)
	want := []string{"a", "b", "c", "d"}

	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func assertContains(t *testing.T, patterns []string, want string) {
	t.Helper()
	for _, p := range patterns {
		if p == want {
			return
		}
	}
	t.Errorf("patterns %v does not contain %q", patterns, want)
}
