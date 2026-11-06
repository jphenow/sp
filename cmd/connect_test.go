package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jphenow/sp/internal/sprite"
)

// TestEnsureIdleTimeout covers when sp changes a sprite's idle timeout and
// when it leaves it alone. The listing omits the field until someone sets it,
// so an absent value is the 30-second default and has to be treated as too
// short — and a sprite already configured for longer must not be rewritten on
// every connect.
func TestEnsureIdleTimeout(t *testing.T) {
	ms := func(d time.Duration) *int {
		v := int(d.Milliseconds())
		return &v
	}
	cases := []struct {
		name   string
		info   *sprite.Info
		update bool
	}{
		{"never set", &sprite.Info{}, true},
		{"config without a timeout", &sprite.Info{Config: &sprite.Config{}}, true},
		{"shorter than sp wants", &sprite.Info{Config: &sprite.Config{IdleTimeoutMS: ms(time.Minute)}}, true},
		{"no listing to go on", nil, true},
		{"exactly what sp wants", &sprite.Info{Config: &sprite.Config{IdleTimeoutMS: ms(sessionIdleTimeout)}}, false},
		{"longer than sp wants", &sprite.Info{Config: &sprite.Config{IdleTimeoutMS: ms(time.Hour)}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			called := filepath.Join(dir, "args")
			fake := filepath.Join(dir, "sprite")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+called+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			ensureIdleTimeout(sprite.NewClient(""), "live", tc.info)

			args, err := os.ReadFile(called)
			if !tc.update {
				if err == nil {
					t.Errorf("rewrote a timeout that was already long enough: %s", args)
				}
				return
			}
			if err != nil {
				t.Fatal("the sprite CLI was never called")
			}
			if !strings.Contains(string(args), `"idle_timeout_ms":900000`) {
				t.Errorf("args = %s, want the 15 minute timeout", args)
			}
			if !strings.Contains(string(args), "-X PUT") {
				t.Errorf("args = %s, want a PUT", args)
			}
		})
	}
}
