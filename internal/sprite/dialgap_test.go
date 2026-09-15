package sprite

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Built from a real sprite CLI debug log of a 135s call: all local work is
// instant and one gap before "Command started" accounts for the whole thing.
const sampleLog = `time=2026-08-21T10:51:39.475-05:00 level=DEBUG msg="Loaded user config" userID=x
time=2026-08-21T10:51:39.476-05:00 level=DEBUG msg="Using sprite override" sprite=s
time=2026-08-21T10:51:39.576-05:00 level=DEBUG msg="sprites: control disabled by client option" sprite=s
time=2026-08-21T10:53:54.696-05:00 level=DEBUG msg="Command started" connectionMode=direct
time=2026-08-21T10:53:54.716-05:00 level=DEBUG msg="sprites: non-pty exit" code=0
`

func TestAnalyzeDialGapFindsTheStall(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "dbg-*.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(sampleLog); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got := analyzeDialGap(f.Name(), 10*time.Second)
	if got == "" {
		t.Fatal("expected a gap to be reported")
	}
	if !strings.Contains(got, "control disabled by client option") || !strings.Contains(got, "Command started") {
		t.Errorf("gap should name the bracketing log lines, got %q", got)
	}
	if !strings.Contains(got, "2m15") {
		t.Errorf("gap should report the 135s duration, got %q", got)
	}
}

func TestAnalyzeDialGapIgnoresSmallGaps(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "dbg-*.log")
	f.WriteString(sampleLog)
	f.Close()
	// A minGap above the actual 135s stall should suppress the report.
	if got := analyzeDialGap(f.Name(), 10*time.Minute); got != "" {
		t.Errorf("want no report below threshold, got %q", got)
	}
}

func TestAnalyzeDialGapHandlesUnreadableLog(t *testing.T) {
	if got := analyzeDialGap("/nonexistent/nope.log", time.Second); got != "" {
		t.Errorf("want empty for missing file, got %q", got)
	}
}
