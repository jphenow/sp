package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runRepair executes the real snippet under sh with HOME pointed at a
// constructed layout, and returns what it printed. Running the actual shell is
// the point: the logic lives in shell, so asserting on the Go string would test
// nothing that can break.
func runRepair(t *testing.T, home string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", claudeLauncherRepairScript())
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("snippet failed (it must never fail its host script): %v\n%s", err, out)
	}
	return string(out)
}

// layout builds a sprite-shaped home: a launcher symlink pointing wherever
// linkTarget says, plus the given versions/ entries.
func layout(t *testing.T, linkTarget string, versions map[string]bool) string {
	t.Helper()
	home := t.TempDir()
	mkdirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".local", "share", "sprite-agents", "claude"),
		filepath.Join(home, ".local", "share", "claude", "versions"),
	}
	for _, d := range mkdirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The pinned binary the stale launcher points at.
	pinned := filepath.Join(home, ".local", "share", "sprite-agents", "claude", "claude")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, executable := range versions {
		mode := os.FileMode(0o644)
		if executable {
			mode = 0o755
		}
		p := filepath.Join(home, ".local", "share", "claude", "versions", name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if linkTarget != "" {
		link := filepath.Join(home, ".local", "bin", "claude")
		if err := os.Symlink(strings.ReplaceAll(linkTarget, "$HOME", home), link); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func launcherTarget(t *testing.T, home string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(home, ".local", "bin", "claude"))
	if err != nil {
		return ""
	}
	return target
}

// The version numbers here are the ones actually observed on an affected
// sprite, plus 2.1.9 — a lexical max picks 2.1.9 over 2.1.287.
func TestRepairPicksNewestByVersionOrder(t *testing.T) {
	home := layout(t, "$HOME/.local/share/sprite-agents/claude/claude", map[string]bool{
		"2.1.9": true, "2.1.268": true, "2.1.287": true, "2.1.270": true,
	})
	out := runRepair(t, home)

	want := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.287")
	if got := launcherTarget(t, home); got != want {
		t.Errorf("launcher -> %q, want %q", got, want)
	}
	if !strings.Contains(out, claudeLauncherRepairMarker+"2.1.287") {
		t.Errorf("expected the repair to report the version, got %q", out)
	}
}

// A partially-downloaded version is not executable; pointing the launcher at
// one would leave a claude that can't run.
func TestRepairSkipsNonExecutableVersions(t *testing.T) {
	home := layout(t, "$HOME/.local/share/sprite-agents/claude/claude", map[string]bool{
		"2.1.287": false, "2.1.270": true,
	})
	runRepair(t, home)
	want := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.270")
	if got := launcherTarget(t, home); got != want {
		t.Errorf("launcher -> %q, want the newest EXECUTABLE version %q", got, want)
	}
}

func TestRepairLeavesHealthyAndForeignLaunchersAlone(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		// Already managed: the common case on every connect after the first.
		{"already managed", "$HOME/.local/share/claude/versions/2.1.270"},
		// A wrapper someone put there deliberately.
		{"hand-rolled wrapper", "$HOME/.local/bin/my-claude-wrapper"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := layout(t, c.target, map[string]bool{"2.1.270": true, "2.1.287": true})
			before := launcherTarget(t, home)
			out := runRepair(t, home)
			if got := launcherTarget(t, home); got != before {
				t.Errorf("launcher moved from %q to %q; it should have been left alone", before, got)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("must stay silent when there is nothing to fix, printed %q", out)
			}
		})
	}
}

func TestRepairNoOpsWithoutSomethingToPointAt(t *testing.T) {
	stale := "$HOME/.local/share/sprite-agents/claude/claude"
	cases := map[string]map[string]bool{
		"empty versions dir":     {},
		"only partial downloads": {"2.1.287": false},
	}
	for name, versions := range cases {
		t.Run(name, func(t *testing.T) {
			home := layout(t, stale, versions)
			before := launcherTarget(t, home)
			out := runRepair(t, home)
			if got := launcherTarget(t, home); got != before {
				t.Errorf("launcher changed to %q; with nothing valid to point at it must not move", got)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("expected silence, got %q", out)
			}
		})
	}
}

// No launcher at all: must not create one, and must not error.
func TestRepairWithNoLauncher(t *testing.T) {
	home := layout(t, "", map[string]bool{"2.1.287": true})
	if out := runRepair(t, home); strings.TrimSpace(out) != "" {
		t.Errorf("expected silence, got %q", out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "claude")); !os.IsNotExist(err) {
		t.Error("must not create a launcher where the user has none")
	}
}

// It runs on every connect, so the second run has to be a silent no-op.
func TestRepairIsIdempotent(t *testing.T) {
	home := layout(t, "$HOME/.local/share/sprite-agents/claude/claude", map[string]bool{"2.1.287": true})
	first := runRepair(t, home)
	if !strings.Contains(first, claudeLauncherRepairMarker) {
		t.Fatalf("first run should repair, got %q", first)
	}
	afterFirst := launcherTarget(t, home)

	second := runRepair(t, home)
	if strings.TrimSpace(second) != "" {
		t.Errorf("second run must be silent, got %q", second)
	}
	if got := launcherTarget(t, home); got != afterFirst {
		t.Errorf("second run moved the launcher from %q to %q", afterFirst, got)
	}
}

// A broken symlink still resolves into sprite-agents/, and is exactly the case
// worth fixing — but readlink -f on a dangling link must not derail the script.
func TestRepairHandlesDanglingStaleLauncher(t *testing.T) {
	home := layout(t, "$HOME/.local/share/sprite-agents/claude/claude", map[string]bool{"2.1.287": true})
	if err := os.Remove(filepath.Join(home, ".local", "share", "sprite-agents", "claude", "claude")); err != nil {
		t.Fatal(err)
	}
	runRepair(t, home)
	want := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.287")
	if got := launcherTarget(t, home); got != want {
		t.Errorf("launcher -> %q, want %q", got, want)
	}
}
