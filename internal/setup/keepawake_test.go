package setup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAwakeCommandCarriesNothingToMangle checks the property that keeps the
// hold loop intact on its way to the service: the command holds no quotes or
// backslashes, so no layer of shell parsing can change it.
//
// The loop once arrived with its probe's quoting altered, and the stored probe
// failed on every poll -- so the hold never engaged during real work.
func TestAwakeCommandCarriesNothingToMangle(t *testing.T) {
	cmd := awakeCommand(awakeScript(T3BusyProbe))
	if strings.ContainsAny(cmd, `'"\`) {
		t.Fatalf("awake command contains quoting a shell layer could reinterpret: %q", cmd)
	}
}

// TestAwakeLoopHoldsWhileT3IsBusy runs the generated command end to end, as
// the service would, and checks what it asks of the guest.
//
// Checking the probe on its own passed while the hold never fired, so this
// runs the command whole -- decode, loop, probe -- with stand-ins for the
// guest's keep-awake and for sleep, the latter ending the loop after its
// first poll. It cannot reproduce the mangling on the way to the service;
// TestAwakeCommandCarriesNothingToMangle guards that.
func TestAwakeLoopHoldsWhileT3IsBusy(t *testing.T) {
	cases := []struct {
		name      string
		processes string
		wantHold  bool
	}{
		{"agent running", `[{"pid":547,"startTime":"x","pgid":547}]`, true},
		{"agent idle", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := runAwakeLoopOnce(t, tc.processes)
			held := strings.Contains(calls, "keep-awake --for "+awakeHold)
			if held != tc.wantHold {
				t.Fatalf("hold requested = %v, want %v; guest calls:\n%s", held, tc.wantHold, calls)
			}
		})
	}
}

// runAwakeLoopOnce runs one poll of the hold loop against a fake T3 state
// directory and returns the keep-awake calls it made, one per line.
func runAwakeLoopOnce(t *testing.T, processes string) string {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "calls")

	state := filepath.Join(home, ".t3", "userdata", "provider-processes")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"version":1,"instanceId":"i","processes":` + processes + `}`
	if err := os.WriteFile(filepath.Join(state, "i.json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"sprite-env": "#!/bin/sh\necho \"$*\" >> " + log + "\n",
		// The loop's shell is sleep's parent; ending it ends the loop.
		"sleep": "#!/bin/sh\nkill -TERM $PPID\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/sh", "-c", awakeCommand(awakeScript(T3BusyProbe)))
	c.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	c.Run() // The loop is ended by a signal, so its exit status says nothing.
	if ctx.Err() != nil {
		t.Fatal("the loop did not stop after one poll")
	}

	calls, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(calls)
}
