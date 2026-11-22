package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// TestAwakeLoopAsksForHoldOnlyWhenBusy runs the generated command end to end,
// as the service would, and checks what it asks of the guest.
//
// The probes answer by exiting, which is how T3BusyProbe answers too: if the
// probe ran in the loop's own shell, its exit would end the loop instead.
//
// Checking a probe on its own once passed while the hold never fired, so this
// runs the command whole -- decode, loop, probe -- with stand-ins for the
// guest's keep-awake and for sleep, the latter ending the loop after its first
// poll. It cannot reproduce the mangling on the way to the service;
// TestAwakeCommandCarriesNothingToMangle guards that.
func TestAwakeLoopAsksForHoldOnlyWhenBusy(t *testing.T) {
	for _, tc := range []struct {
		probe    string
		wantHold bool
	}{
		{"exit 0", true},
		{"exit 1", false},
	} {
		t.Run(tc.probe, func(t *testing.T) {
			calls := runAwakeLoopOnce(t, tc.probe)
			held := strings.Contains(calls, "keep-awake --for "+awakeHold)
			if held != tc.wantHold {
				t.Fatalf("hold requested = %v, want %v; guest calls:\n%s", held, tc.wantHold, calls)
			}
		})
	}
}

// runAwakeLoopOnce runs one poll of the hold loop with the given probe and
// returns the keep-awake calls it made, one per line.
func runAwakeLoopOnce(t *testing.T, probe string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "calls")
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
	c := exec.CommandContext(ctx, "/bin/sh", "-c", awakeCommand(awakeScript(probe)))
	c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
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

// TestT3BusyProbe checks the probe against T3 state laid out on disk, with
// real processes standing in for T3's provider and its children.
//
// The case that matters most is "idle provider": T3 keeps its provider process
// alive between turns, and a probe that read that as work held an idle sprite
// awake indefinitely.
func TestT3BusyProbe(t *testing.T) {
	t.Run("idle provider", func(t *testing.T) {
		home := t3State(t, providerWith(t, watchdogChild), nil)
		wantBusy(t, home, false)
	})
	t.Run("command running", func(t *testing.T) {
		home := t3State(t, providerWith(t, commandChild), nil)
		wantBusy(t, home, true)
	})
	t.Run("stale record", func(t *testing.T) {
		// A pid well above anything the test could have started.
		home := t3State(t, 4194000, nil)
		wantBusy(t, home, false)
	})
	t.Run("run in progress", func(t *testing.T) {
		home := t3State(t, providerWith(t, watchdogChild), []string{"2026-11-22T20:41:41Z", "NULL"})
		wantBusy(t, home, true)
	})
	t.Run("runs finished", func(t *testing.T) {
		home := t3State(t, providerWith(t, watchdogChild), []string{"2026-11-22T20:41:41Z"})
		wantBusy(t, home, false)
	})
}

const (
	// watchdogChild is the shape of the watchdog a provider always runs: its
	// command line is what the probe recognises and ignores. The trailing
	// ":" keeps the shell from replacing itself with sleep.
	watchdogChild = `/bin/sh -c 'read -r _ <&4; sleep 30; :' & wait`
	// commandChild is a provider running a tool command.
	commandChild = `sleep 30 & wait`
)

// providerWith starts a stand-in provider process running script, waits for
// its child to appear, and returns its pid. It is killed when the test ends.
func providerWith(t *testing.T, script string) int {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", script)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		c.Wait()
	})
	for i := 0; i < 50; i++ {
		out, _ := exec.Command("pgrep", "-P", fmt.Sprint(c.Process.Pid)).Output()
		if len(strings.TrimSpace(string(out))) > 0 {
			return c.Process.Pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stand-in provider never started its child")
	return 0
}

// t3State lays out a T3 state directory under a fresh home: one provider
// record naming pid, and, when runs is non-nil, a state database holding a
// run per entry with that completed_at ("NULL" for one still in progress).
func t3State(t *testing.T, pid int, runs []string) string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, ".t3", "userdata")
	procs := filepath.Join(state, "provider-processes")
	if err := os.MkdirAll(procs, 0o755); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"version":1,"owner":{"pid":1},"processes":[{"pid":%d,"pgid":%d}]}`, pid, pid)
	if err := os.WriteFile(filepath.Join(procs, "i.json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	if runs == nil {
		return home
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	sql := "create table orchestration_v2_projection_runs (run_id text, completed_at text);"
	for i, at := range runs {
		if at != "NULL" {
			at = "'" + at + "'"
		}
		sql += fmt.Sprintf("insert into orchestration_v2_projection_runs values ('r%d', %s);", i, at)
	}
	if out, err := exec.Command("sqlite3", filepath.Join(state, "statev2.sqlite"), sql).CombinedOutput(); err != nil {
		t.Fatalf("building state database: %v\n%s", err, out)
	}
	return home
}

// wantBusy runs T3BusyProbe with home as $HOME and checks its answer.
func wantBusy(t *testing.T, home string, want bool) {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", T3BusyProbe)
	c.Env = append(os.Environ(), "HOME="+home)
	err := c.Run()
	if busy := err == nil; busy != want {
		t.Fatalf("probe says busy = %v, want %v (%v)", busy, want, err)
	}
}
