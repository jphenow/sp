package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestT3BinPicksNewestVersion checks that the newest runtime wins when a
// version's names sort differently as text, as 0.1.2 and 0.1.11 do.
func TestT3BinPicksNewestVersion(t *testing.T) {
	home := t.TempDir()
	for _, v := range []string{"0.1.11", "0.1.2", "0.1.9"} {
		dir := filepath.Join(home, ".t3", "runtime", "versions", v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "t3"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := exec.Command("sh", "-c", "echo "+t3Bin)
	c.Env = append(os.Environ(), "HOME="+home)
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasSuffix(got, "/versions/0.1.11/t3") {
		t.Fatalf("picked %q, want 0.1.11", got)
	}
}

// TestT3ServiceServesAnEndpoint checks the T3 service registers an HTTP
// endpoint, and the same port T3 is told to serve on.
func TestT3ServiceServesAnEndpoint(t *testing.T) {
	create := t3ServiceCreate("HOME=/home/sprite")
	for _, want := range []string{"--http-port 3773 ", "--port 3773"} {
		if !strings.Contains(create, want) {
			t.Errorf("missing %q in %s", want, create)
		}
	}
}
