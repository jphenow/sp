package cmd

import (
	"testing"
	"time"
)

// TestStallDetectorIdleIsNotStalled checks that a connection nobody is typing
// into is never called dead. This is the case a plain output watchdog gets
// wrong: a quiet build looks exactly like a hang.
func TestStallDetectorIdleIsNotStalled(t *testing.T) {
	d := &stallDetector{grace: 10 * time.Second}
	start := time.Now()
	if d.stalled(start.Add(time.Hour)) {
		t.Fatal("an idle connection with no input was reported as stalled")
	}
}

// TestStallDetectorAnsweredInput checks that input followed by output clears
// the wait, however long the session then sits quiet.
func TestStallDetectorAnsweredInput(t *testing.T) {
	d := &stallDetector{grace: 10 * time.Second}
	start := time.Now()
	d.sentInput(start)
	d.sawOutput()
	if d.stalled(start.Add(time.Hour)) {
		t.Fatal("input that was answered still counted as outstanding")
	}
}

// TestStallDetectorUnansweredInput checks the actual failure: keystrokes go
// out, nothing comes back, and the grace period expires.
func TestStallDetectorUnansweredInput(t *testing.T) {
	d := &stallDetector{grace: 10 * time.Second}
	start := time.Now()
	d.sentInput(start)

	if d.stalled(start.Add(9 * time.Second)) {
		t.Fatal("called stalled before the grace period elapsed")
	}
	if !d.stalled(start.Add(10 * time.Second)) {
		t.Fatal("unanswered input past the grace period was not reported")
	}
}

// TestStallDetectorKeyRepeatDoesNotDeferDeadline checks that more typing does
// not reset the clock. Holding a key down on a dead connection would otherwise
// keep renewing the deadline and the stall would never be noticed.
func TestStallDetectorKeyRepeatDoesNotDeferDeadline(t *testing.T) {
	d := &stallDetector{grace: 10 * time.Second}
	start := time.Now()
	for i := 0; i < 20; i++ {
		d.sentInput(start.Add(time.Duration(i) * 500 * time.Millisecond))
	}
	if !d.stalled(start.Add(10 * time.Second)) {
		t.Fatal("repeated input pushed the deadline past the grace period")
	}
}

// TestStallDetectorResumesAfterOutput checks that a connection which answers
// late is given a full grace period again, rather than staying condemned.
func TestStallDetectorResumesAfterOutput(t *testing.T) {
	d := &stallDetector{grace: 10 * time.Second}
	start := time.Now()
	d.sentInput(start)
	d.sawOutput()
	d.sentInput(start.Add(5 * time.Second))

	if d.stalled(start.Add(10 * time.Second)) {
		t.Fatal("the second keystroke inherited the first one's deadline")
	}
	if !d.stalled(start.Add(15 * time.Second)) {
		t.Fatal("the second keystroke never reached its own deadline")
	}
}
