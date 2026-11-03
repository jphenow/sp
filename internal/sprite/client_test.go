package sprite

import (
	"slices"
	"testing"
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
