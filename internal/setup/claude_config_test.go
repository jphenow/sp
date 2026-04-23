package setup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestPushClaudeConfigTarMergeSemantics verifies the core contract of
// PushClaudeConfig: when the tar is extracted on the sprite, files that
// exist ONLY on the sprite must be preserved. Locally present files
// overwrite the sprite's version. This test doesn't require a real
// sprite — it exercises the tar building on one side and a manual tar
// extract on the other, asserting the resulting tree.
func TestPushClaudeConfigTarMergeSemantics(t *testing.T) {
	localHome := t.TempDir()
	localClaude := filepath.Join(localHome, ".claude")
	mustMkdirAll(t, filepath.Join(localClaude, "skills", "local-skill"))
	mustMkdirAll(t, filepath.Join(localClaude, "skills", "shared-skill"))
	mustMkdirAll(t, filepath.Join(localClaude, "commands"))
	mustWrite(t, filepath.Join(localClaude, "CLAUDE.md"), "LOCAL CLAUDE.md")
	mustWrite(t, filepath.Join(localClaude, "settings.json"), `{"from":"local"}`)
	mustWrite(t, filepath.Join(localClaude, "skills", "local-skill", "skill.md"), "local-skill content")
	mustWrite(t, filepath.Join(localClaude, "skills", "shared-skill", "skill.md"), "LOCAL VERSION")
	mustWrite(t, filepath.Join(localClaude, "commands", "local-cmd.md"), "local command")

	// Simulated sprite home with a mix of content: some that overlaps
	// with local (should be overwritten) and some sprite-only (must be
	// preserved across the extract).
	spriteHome := t.TempDir()
	spriteClaude := filepath.Join(spriteHome, ".claude")
	mustMkdirAll(t, filepath.Join(spriteClaude, "skills", "shared-skill"))
	mustMkdirAll(t, filepath.Join(spriteClaude, "skills", "sprite-only-skill"))
	mustMkdirAll(t, filepath.Join(spriteClaude, "commands"))
	mustWrite(t, filepath.Join(spriteClaude, "CLAUDE.md"), "SPRITE CLAUDE.md")
	mustWrite(t, filepath.Join(spriteClaude, "skills", "shared-skill", "skill.md"), "SPRITE VERSION")
	mustWrite(t, filepath.Join(spriteClaude, "skills", "shared-skill", "sprite-extra.md"), "only on sprite, inside shared-skill")
	mustWrite(t, filepath.Join(spriteClaude, "skills", "sprite-only-skill", "skill.md"), "sprite-only content")
	mustWrite(t, filepath.Join(spriteClaude, "commands", "sprite-cmd.md"), "sprite command")

	// Build the tar from local (the same way PushClaudeConfig does),
	// then extract into sprite.
	tarPath := buildClaudeConfigTar(t, localClaude)
	extractTar(t, tarPath, spriteClaude)

	// Assertions — local must have overwritten shared files.
	assertContent(t, filepath.Join(spriteClaude, "CLAUDE.md"), "LOCAL CLAUDE.md")
	assertContent(t, filepath.Join(spriteClaude, "settings.json"), `{"from":"local"}`)
	assertContent(t, filepath.Join(spriteClaude, "skills", "shared-skill", "skill.md"), "LOCAL VERSION")
	assertContent(t, filepath.Join(spriteClaude, "skills", "local-skill", "skill.md"), "local-skill content")
	assertContent(t, filepath.Join(spriteClaude, "commands", "local-cmd.md"), "local command")

	// And sprite-only content must be preserved — this is the whole
	// point of the merge contract.
	assertContent(t, filepath.Join(spriteClaude, "skills", "sprite-only-skill", "skill.md"), "sprite-only content")
	assertContent(t, filepath.Join(spriteClaude, "commands", "sprite-cmd.md"), "sprite command")
	// A file added by the sprite INSIDE a directory that also exists
	// locally must also survive — tar doesn't empty dirs, it just writes
	// the entries in the archive.
	assertContent(t, filepath.Join(spriteClaude, "skills", "shared-skill", "sprite-extra.md"), "only on sprite, inside shared-skill")
}

// buildClaudeConfigTar constructs a tar.gz from the allow-listed entries
// of a local .claude dir, using the same walker PushClaudeConfig uses.
// Returns the path to the tempfile; caller is responsible for it surviving
// the test run (t.TempDir() in the caller handles cleanup indirectly).
func buildClaudeConfigTar(t *testing.T, claudeDir string) string {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "claude-config-*.tar.gz")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer tmp.Close()
	gw := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gw)
	for _, name := range claudeConfigAllowlist {
		p := filepath.Join(claudeDir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := addToTarFollowingSymlinks(tw, claudeDir, p); err != nil {
			t.Fatalf("adding %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return tmp.Name()
}

// extractTar extracts a tar.gz into a destination directory, mirroring
// what `tar xzf` does on the sprite side: it writes the files in the
// archive and leaves any pre-existing sibling files untouched.
func extractTar(t *testing.T, tarPath, destDir string) {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		target := filepath.Join(destDir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir parent %s: %v", target, err)
			}
			out, err := os.Create(target)
			if err != nil {
				t.Fatalf("create %s: %v", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				t.Fatalf("copy %s: %v", target, err)
			}
			out.Close()
		}
	}
}

// mustMkdirAll creates a dir and fails the test on error.
func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

// mustWrite writes a file and fails the test on error.
func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// assertContent reads a file and fails if it doesn't match expected.
func assertContent(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if string(got) != want {
		t.Errorf("%s content mismatch:\n  got:  %q\n  want: %q", p, string(got), want)
	}
}
