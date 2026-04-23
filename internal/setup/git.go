package setup

import (
	"os/exec"
)

// runGitCommand runs a git command and returns its output.
func runGitCommand(args ...string) ([]byte, error) {
	return exec.Command("git", args...).Output()
}
