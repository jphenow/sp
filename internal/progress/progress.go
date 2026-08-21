// Package progress renders a multi-line progress display for a set of
// concurrently-running setup tasks. Falls back to line-by-line logging
// when stderr isn't a TTY or when verbose mode is on, so the same code
// path works in interactive shells, scripts, and CI.
//
// The package is intentionally small and dependency-light: just lipgloss
// (which is already in this repo's deps for the TUI) for color output and
// the standard library for ANSI cursor control. No bubbletea, no spinner
// libraries — the rendering is a few lines of \033[<n>A escape sequences.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// TaskStatus is the lifecycle state of a single task within a Group.
type TaskStatus int

const (
	// StatusPending means the task has been registered but not started.
	StatusPending TaskStatus = iota
	// StatusRunning means the task's function is currently executing.
	StatusRunning
	// StatusOK means the task completed without error.
	StatusOK
	// StatusFail means the task's function returned an error.
	StatusFail
)

// Task is one unit of work in a Group. Caller doesn't construct it
// directly — Add() returns one for inspection if needed.
type Task struct {
	Name    string
	Status  TaskStatus
	Err     error
	Started time.Time
	Ended   time.Time

	fn func() error

	detailMu sync.Mutex
	detail   string
}

// SetDetail attaches a short live note to a running task, rendered after the
// name ("Waiting for sprite — attempt 3, last: exec timeout"). Safe to call
// from inside the task's own function while the renderer is drawing.
func (t *Task) SetDetail(s string) {
	t.detailMu.Lock()
	t.detail = s
	t.detailMu.Unlock()
}

// Detail returns the current live note, or "".
func (t *Task) Detail() string {
	t.detailMu.Lock()
	defer t.detailMu.Unlock()
	return t.detail
}

// Duration returns how long the task has been running, or its total
// duration if completed. Zero for pending tasks.
func (t *Task) Duration() time.Duration {
	if t.Started.IsZero() {
		return 0
	}
	end := t.Ended
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(t.Started)
}

// Group is a set of tasks that run in parallel and render their state
// to stderr. Use Add() to register tasks, then Run() to execute and
// block until completion. Run() returns the first error encountered
// (other tasks still finish).
type Group struct {
	out     io.Writer
	isTTY   bool
	verbose bool

	mu       sync.Mutex
	tasks    []*Task
	rendered int // line count of the previous frame, for in-place redraw
	footer   func() []string
}

// SetFooter installs a callback rendered as extra dim lines below the task
// list on every frame. Used to show which sprite calls are currently in flight
// — without it a stalled task is just a spinner, with no way to tell whether
// it's stuck on a dial, on which command, or for how long.
//
// The callback runs on the render goroutine at every frame, so it must be
// cheap and must not block.
func (g *Group) SetFooter(fn func() []string) {
	g.mu.Lock()
	g.footer = fn
	g.mu.Unlock()
}

// New constructs a Group rendering to stderr. If verbose is true OR
// stderr isn't a terminal, the group skips the spinner and emits
// line-by-line lifecycle logs ("[start]", "[ok]", "[fail]") instead.
func New(verbose bool) *Group {
	isTTY := isTerminalWriter(os.Stderr)
	return &Group{
		out:     os.Stderr,
		isTTY:   isTTY,
		verbose: verbose || !isTTY,
	}
}

// Add registers a task. Tasks don't run until Run() is called. The
// returned *Task is mostly for tests; production callers can ignore it.
func (g *Group) Add(name string, fn func() error) *Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := &Task{Name: name, fn: fn, Status: StatusPending}
	g.tasks = append(g.tasks, t)
	return t
}

// Run executes every registered task in its own goroutine and blocks
// until all complete. Returns the first error encountered (other tasks
// still finish before Run returns). The group is not reusable after Run.
func (g *Group) Run() error {
	if len(g.tasks) == 0 {
		return nil
	}

	g.render() // initial draw of pending state

	var rendererStop chan struct{}
	if !g.verbose {
		rendererStop = make(chan struct{})
		go g.renderLoop(rendererStop)
	}

	var wg sync.WaitGroup
	for _, t := range g.tasks {
		wg.Add(1)
		go func(task *Task) {
			defer wg.Done()
			g.markRunning(task)
			err := safeRun(task.fn)
			g.markDone(task, err)
		}(t)
	}
	wg.Wait()

	if rendererStop != nil {
		close(rendererStop)
	}
	g.render() // final frame so the spinner glyphs are replaced with ✓/✗

	g.mu.Lock()
	defer g.mu.Unlock()
	for _, t := range g.tasks {
		if t.Err != nil {
			return t.Err
		}
	}
	return nil
}

// RunSequential executes tasks one at a time, in registration order, stopping
// at the first failure. Use it for steps that depend on each other — they get
// their own progress line each (so a slow step is attributable) without the
// caller having to hide the ordering inside one opaque task.
//
// Remaining tasks stay pending and are rendered as such when one fails.
func (g *Group) RunSequential() error {
	if len(g.tasks) == 0 {
		return nil
	}

	g.render()

	var rendererStop chan struct{}
	if !g.verbose {
		rendererStop = make(chan struct{})
		go g.renderLoop(rendererStop)
	}

	var firstErr error
	for _, t := range g.tasks {
		g.markRunning(t)
		err := safeRun(t.fn)
		g.markDone(t, err)
		if err != nil {
			firstErr = err
			break
		}
	}

	if rendererStop != nil {
		close(rendererStop)
	}
	g.render()
	return firstErr
}

// Failures returns every task that failed, in registration order. Run() only
// returns the first error, which understates things badly when an API outage
// takes down several tasks at once — the progress line truncates each error to
// fit the terminal, so this is the only place the full text of the others is
// still reachable.
func (g *Group) Failures() []*Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	var failed []*Task
	for _, t := range g.tasks {
		if t.Status == StatusFail {
			failed = append(failed, t)
		}
	}
	return failed
}

// safeRun catches a panic in a task function and converts it to an error
// so a single broken task doesn't take down the whole connect flow.
func safeRun(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

// markRunning records a task transition to running and emits a verbose
// log line if applicable.
func (g *Group) markRunning(t *Task) {
	g.mu.Lock()
	t.Status = StatusRunning
	t.Started = time.Now()
	g.mu.Unlock()
	if g.verbose {
		fmt.Fprintf(g.out, "[start] %s\n", t.Name)
	}
}

// markDone records a task's terminal state and emits a verbose log line
// if applicable.
func (g *Group) markDone(t *Task, err error) {
	g.mu.Lock()
	t.Ended = time.Now()
	if err != nil {
		t.Status = StatusFail
		t.Err = err
	} else {
		t.Status = StatusOK
	}
	g.mu.Unlock()
	if g.verbose {
		if err != nil {
			fmt.Fprintf(g.out, "[fail]  %s (%s): %v\n", t.Name, t.Duration().Round(time.Millisecond), err)
		} else {
			fmt.Fprintf(g.out, "[ok]    %s (%s)\n", t.Name, t.Duration().Round(time.Millisecond))
		}
	}
}

const renderInterval = 80 * time.Millisecond

// spinnerFrames is the braille spinner used for the running indicator.
// Same as the one bubbles/spinner uses by default — looks fine in most
// monospaced fonts and supports cleaner subpixel motion than ASCII.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// renderLoop redraws the task list at a fixed interval until stop is
// closed. Each tick triggers one render() call which locks the mutex
// briefly to read task state.
func (g *Group) renderLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(renderInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			g.render()
		}
	}
}

// render draws the current task list. In TTY+non-verbose mode it moves
// the cursor up to the top of the previous frame and overwrites it; in
// other modes it's a no-op (those modes use lifecycle log lines from
// markRunning/markDone instead).
func (g *Group) render() {
	if g.verbose {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	var sb strings.Builder
	if g.rendered > 0 {
		// Move cursor up to the start of the previous frame.
		fmt.Fprintf(&sb, "\033[%dA\r", g.rendered)
	}

	// One spinner frame for all running tasks per render — keeps them
	// visually in sync rather than each having its own phase.
	frame := int(time.Now().UnixMilli()/int64(renderInterval/time.Millisecond)) % len(spinnerFrames)

	for _, t := range g.tasks {
		sb.WriteString("\033[K") // clear to end of line
		switch t.Status {
		case StatusPending:
			sb.WriteString(dimStyle.Render("⏳ "))
			sb.WriteString(t.Name)
		case StatusRunning:
			sb.WriteString(runStyle.Render(spinnerFrames[frame] + " "))
			sb.WriteString(t.Name)
			sb.WriteString(dimStyle.Render(" " + t.Duration().Round(100*time.Millisecond).String()))
			if d := t.Detail(); d != "" {
				sb.WriteString(dimStyle.Render(" — " + d))
			}
		case StatusOK:
			sb.WriteString(okStyle.Render("✓ "))
			sb.WriteString(t.Name)
			sb.WriteString(dimStyle.Render(" " + t.Duration().Round(time.Millisecond).String()))
			if d := t.Detail(); d != "" {
				sb.WriteString(dimStyle.Render(" — " + d))
			}
		case StatusFail:
			sb.WriteString(failStyle.Render("✗ "))
			sb.WriteString(t.Name)
			if t.Err != nil {
				sb.WriteString(failStyle.Render(": " + summarizeErr(t.Err)))
			}
		}
		sb.WriteByte('\n')
	}

	lines := len(g.tasks)
	if g.footer != nil {
		for _, l := range g.footer() {
			sb.WriteString("\033[K")
			sb.WriteString(dimStyle.Render("  " + l))
			sb.WriteByte('\n')
			lines++
		}
	}
	// Clear any lines the previous frame drew that this one doesn't, so a
	// shrinking footer doesn't leave stale text behind.
	for i := lines; i < g.rendered; i++ {
		sb.WriteString("\033[K\n")
		lines++
	}

	g.out.Write([]byte(sb.String()))
	g.rendered = lines
}

// summarizeErr trims long multi-line errors to a single line for the
// progress display. Full error text is still returned by Run().
func summarizeErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	const maxLen = 80
	if len(s) > maxLen {
		s = s[:maxLen-1] + "…"
	}
	return s
}

var (
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green
	failStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
	runStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // cyan
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // gray
)

// isTerminalWriter reports whether w is an *os.File pointing at a TTY.
// Used to decide between spinner mode and line-by-line lifecycle logs.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
