package sprite

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Span is one timed invocation of the sprite CLI.
type Span struct {
	Op     string // "exec", "create", "api", "destroy"
	Detail string // redacted, human-readable summary of what ran
	Dur    time.Duration
	Failed bool
}

var (
	traceMu    sync.Mutex
	traceSpans []Span
)

// record appends a span. Always on: the cost is a mutex and a small struct
// per sprite-CLI call, against calls that take 0.25s warm and 30s+ cold.
// Collecting unconditionally is the whole point — the slow runs are the ones
// you can't reproduce on demand with a flag.
func record(op, detail string, start time.Time, err error) {
	traceMu.Lock()
	defer traceMu.Unlock()
	traceSpans = append(traceSpans, Span{
		Op:     op,
		Detail: detail,
		Dur:    time.Since(start),
		Failed: err != nil,
	})
}

// Spans returns a copy of everything recorded so far.
func Spans() []Span {
	traceMu.Lock()
	defer traceMu.Unlock()
	return append([]Span(nil), traceSpans...)
}

// TotalTraced returns the call count and summed duration of all spans. Note
// the sum exceeds wall-clock time when calls ran concurrently — it measures
// work, not elapsed time.
func TotalTraced() (int, time.Duration) {
	traceMu.Lock()
	defer traceMu.Unlock()
	var total time.Duration
	for _, s := range traceSpans {
		total += s.Dur
	}
	return len(traceSpans), total
}

// slowestN is how many spans TraceSummary lists individually.
const slowestN = 5

// TraceSummary renders the latency profile of this run: call count, summed
// duration, and the slowest few calls. Returns "" when nothing was recorded.
//
// The summed duration is deliberately reported alongside the count, because
// the failure mode this exists to diagnose is "one cold-start wake cost 30s"
// versus "twenty warm calls cost 0.25s each" — those look identical from the
// outside and completely different here.
func TraceSummary() string {
	spans := Spans()
	if len(spans) == 0 {
		return ""
	}
	var total time.Duration
	byOp := map[string]time.Duration{}
	countByOp := map[string]int{}
	for _, s := range spans {
		total += s.Dur
		byOp[s.Op] += s.Dur
		countByOp[s.Op]++
	}

	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Dur > spans[j].Dur })

	var b strings.Builder
	fmt.Fprintf(&b, "sprite CLI calls: %d, %s total (concurrent — sums past wall clock)\n",
		len(spans), total.Round(time.Millisecond))

	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		fmt.Fprintf(&b, "  %-8s %3d calls  %s\n", op, countByOp[op], byOp[op].Round(time.Millisecond))
	}

	n := slowestN
	if len(spans) < n {
		n = len(spans)
	}
	fmt.Fprintf(&b, "  slowest %d:\n", n)
	for _, s := range spans[:n] {
		mark := " "
		if s.Failed {
			mark = "!"
		}
		fmt.Fprintf(&b, "   %s %8s  %s\n", mark, s.Dur.Round(time.Millisecond), s.Detail)
	}
	return b.String()
}

// ResetTrace clears recorded spans. For tests.
func ResetTrace() {
	traceMu.Lock()
	defer traceMu.Unlock()
	traceSpans = nil
}

// secretish matches long unbroken base64/token-shaped runs. Several exec
// scripts embed credentials directly (PushClaudeCredentials base64s the OAuth
// blob into its shell command), so nothing derived from a command string may
// be printed without stripping these first.
var secretish = regexp.MustCompile(`[A-Za-z0-9+/_-]{40,}={0,2}`)

// describeExec builds a short, redacted label for an exec call. It uses the
// first non-empty line of the command — for `sh -c <script>` that's the first
// real line of the script, which is usually recognizable — plus the upload
// destinations, which are useful and never secret.
//
// Env is deliberately excluded: it carries GH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN.
func describeExec(opts ExecOptions) string {
	cmd := strings.Join(opts.Command, " ")
	// For `sh -c <script>` the interesting part is the script, not the "sh -c"
	// prefix — and joining first would make every one of these look identical.
	if len(opts.Command) == 3 && opts.Command[0] == "sh" && opts.Command[1] == "-c" {
		cmd = opts.Command[2]
	}
	line := ""
	for _, l := range strings.Split(cmd, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	line = secretish.ReplaceAllString(line, "<redacted>")
	const maxLen = 70
	if len(line) > maxLen {
		line = line[:maxLen-1] + "…"
	}
	if len(opts.Files) > 0 {
		dests := make([]string, 0, len(opts.Files))
		for _, remote := range opts.Files {
			dests = append(dests, remote)
		}
		sort.Strings(dests)
		line = fmt.Sprintf("upload[%s] %s", strings.Join(dests, ","), line)
	}
	if opts.Sprite != "" {
		return opts.Sprite + ": " + line
	}
	return line
}
