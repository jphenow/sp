package progress

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunAllOK exercises the basic happy path: every task succeeds, and
// Run reports no error.
func TestRunAllOK(t *testing.T) {
	g := New(true) // verbose so render() is a no-op (no TTY in tests)
	var counter int32
	for i := 0; i < 4; i++ {
		g.Add("task", func() error {
			atomic.AddInt32(&counter, 1)
			time.Sleep(5 * time.Millisecond)
			return nil
		})
	}
	if err := g.Run(); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := atomic.LoadInt32(&counter); got != 4 {
		t.Errorf("expected 4 tasks to run, got %d", got)
	}
}

// TestRunReturnsFirstError verifies Run waits for all tasks but returns
// the error from the first failing task it encounters in the task list.
func TestRunReturnsFirstError(t *testing.T) {
	g := New(true)
	bonk := errors.New("bonk")
	var ran int32
	g.Add("ok", func() error {
		atomic.AddInt32(&ran, 1)
		return nil
	})
	g.Add("fail", func() error {
		atomic.AddInt32(&ran, 1)
		return bonk
	})
	g.Add("ok2", func() error {
		atomic.AddInt32(&ran, 1)
		return nil
	})
	err := g.Run()
	if !errors.Is(err, bonk) {
		t.Errorf("expected bonk, got %v", err)
	}
	// All tasks should have run even though one failed.
	if got := atomic.LoadInt32(&ran); got != 3 {
		t.Errorf("expected 3 tasks to run, got %d", got)
	}
}

// TestPanicConvertedToError verifies that a panicking task doesn't take
// down the entire Run, and the panic surfaces as an error on that task.
func TestPanicConvertedToError(t *testing.T) {
	g := New(true)
	g.Add("good", func() error { return nil })
	g.Add("bad", func() error { panic("kaboom") })
	g.Add("good2", func() error { return nil })
	err := g.Run()
	if err == nil || err.Error() != "panic: kaboom" {
		t.Errorf("expected panic error, got %v", err)
	}
}

// TestEmptyGroup verifies Run is a no-op for an empty group.
func TestEmptyGroup(t *testing.T) {
	g := New(true)
	if err := g.Run(); err != nil {
		t.Errorf("expected nil error from empty group, got %v", err)
	}
}

// TestParallelExecution verifies tasks really do run concurrently —
// total wall time should be ~ the longest task, not the sum.
func TestParallelExecution(t *testing.T) {
	g := New(true)
	const taskDur = 30 * time.Millisecond
	for i := 0; i < 5; i++ {
		g.Add("task", func() error {
			time.Sleep(taskDur)
			return nil
		})
	}
	start := time.Now()
	if err := g.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)
	// Generous bound: serial would be 150ms, parallel should be < 60ms.
	if elapsed > 100*time.Millisecond {
		t.Errorf("tasks did not run in parallel: %v", elapsed)
	}
}

// TestSummarizeErr exercises the multi-line/long-line error trimming
// used in the spinner display.
func TestSummarizeErr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"short", "short"},
		{"line one\nline two", "line one…"},
		{"a very very very very very very very very very very very very very very very very very long error", "a very very very very very very very very very very very very very very very ve…"},
	}
	for _, c := range cases {
		got := summarizeErr(errors.New(c.in))
		if got != c.want {
			t.Errorf("summarizeErr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
