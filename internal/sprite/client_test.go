package sprite

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestBuildExecArgsStdin covers the stdin declaration. Leaving it off made the
// CLI treat sp's /dev/null stdin as piped input, which disabled its
// reconnection and turned any blip during setup into a failed task.
func TestBuildExecArgsStdin(t *testing.T) {
	c := &Client{}

	t.Run("a non-TTY exec declares it wants no stdin", func(t *testing.T) {
		args := c.BuildExecArgs(ExecOptions{Sprite: "blue", Command: []string{"true"}})
		if !slices.Contains(args, "--no-stdin") {
			t.Fatalf("missing --no-stdin: %v", args)
		}
	})

	t.Run("a TTY exec does not, because the CLI rejects the pair", func(t *testing.T) {
		args := c.BuildExecArgs(ExecOptions{Sprite: "blue", TTY: true, Command: []string{"bash"}})
		if slices.Contains(args, "--no-stdin") {
			t.Fatalf("--no-stdin is not supported with --tty: %v", args)
		}
	})
}

// TestSetIdleTimeoutUnsupported covers a sprite that has no such setting. The
// API answers with an NDJSON error event inside an exit-0 response, and the
// distinction matters: sp warns about a refusal it might fix, and says nothing
// about a sprite that was never going to accept the call.
func TestSetIdleTimeoutUnsupported(t *testing.T) {
	dir := t.TempDir()
	body := `{"error":{"code":"not_provider_sprite","details":{"id":"s"},` +
		`"message":"This operation requires a provider-backed sprite."},"type":"error"}`
	script := "#!/bin/sh\ncat <<'JSON'\n" + body + "\nJSON\n"
	if err := os.WriteFile(filepath.Join(dir, "sprite"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := NewClient("").SetIdleTimeout("s", 15*time.Minute)
	if !errors.Is(err, ErrIdleTimeoutUnsupported) {
		t.Fatalf("SetIdleTimeout = %v, want ErrIdleTimeoutUnsupported", err)
	}
}
