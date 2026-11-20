package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTailnetHostnameFor checks that a sprite name survives as a hostname and
// that a name a hostname cannot hold is reduced to one that it can.
func TestTailnetHostnameFor(t *testing.T) {
	cases := map[string]string{
		"gameservers":            "gameservers",
		"jphenow--sp":            "jphenow--sp",
		"jphenow--sp--variant":   "jphenow--sp--variant",
		"has spaces":             "has-spaces",
		"under_score":            "under-score",
		"dots.and/slashes":       "dots-and-slashes",
		"-leading-and-trailing-": "leading-and-trailing",
	}
	for in, want := range cases {
		if got := tailnetHostnameFor(in); got != want {
			t.Errorf("tailnetHostnameFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTailscaleAuthKey checks the three states that decide whether sprites
// join a tailnet at all: an environment key, a key on disk, and neither.
//
// The absent case is the one that matters most. An empty string is how
// somebody who has never heard of this feature opts out of it, so a missing
// file must read as "no key" and not as an error.
func TestTailscaleAuthKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TS_AUTHKEY", "")

	if got := TailscaleAuthKey(); got != "" {
		t.Fatalf("with no key configured, got %q, want \"\"", got)
	}

	keyPath := filepath.Join(home, ".config", "sprite", "tailscale.authkey")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Trailing newline included deliberately: this file gets created by a
	// shell redirect or an editor more often than not.
	if err := os.WriteFile(keyPath, []byte("tskey-auth-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TailscaleAuthKey(); got != "tskey-auth-from-file" {
		t.Errorf("from file, got %q, want %q", got, "tskey-auth-from-file")
	}

	t.Setenv("TS_AUTHKEY", "tskey-auth-from-env")
	if got := TailscaleAuthKey(); got != "tskey-auth-from-env" {
		t.Errorf("the environment must win over the file, got %q", got)
	}
}

// TestTailscaleTags checks the tag default and, more importantly, that there
// is a way to say "no tags at all" -- owning a tag means editing the tailnet's
// policy file, and that cannot be a precondition for trying this once.
func TestTailscaleTags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TS_TAGS", "")

	if got := TailscaleTags(); got != "tag:sprite" {
		t.Errorf("with nothing configured, got %q, want %q", got, "tag:sprite")
	}

	tagsPath := filepath.Join(home, ".config", "sprite", "tailscale.tags")
	if err := os.MkdirAll(filepath.Dir(tagsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ file, want string }{
		{"tag:dev\n", "tag:dev"},
		{"tag:dev,tag:ci", "tag:dev,tag:ci"},
		{"none\n", ""},
		{"None", ""},
		{"  \n", "tag:sprite"}, // an empty file is "unset", not "untagged"
	} {
		if err := os.WriteFile(tagsPath, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := TailscaleTags(); got != tc.want {
			t.Errorf("tags file %q: got %q, want %q", tc.file, got, tc.want)
		}
	}

	t.Setenv("TS_TAGS", "tag:from-env")
	if got := TailscaleTags(); got != "tag:from-env" {
		t.Errorf("the environment must win over the file, got %q", got)
	}
}
