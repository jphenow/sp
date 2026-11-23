package keepalive

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDetachedCommandOutlivesCaller checks that the command returns at once and
// the script keeps running after it, with its own quoting intact.
func TestDetachedCommandOutlivesCaller(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	out := filepath.Join(t.TempDir(), "done")
	argv := DetachedCommand(`sleep 1; echo "it's still here" > '` + out + `'`)

	start := time.Now()
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("command waited %s for the script; want it to return at once", took)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && string(b) == "it's still here\n" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("script did not run to completion after the command returned")
}
