// Package hub implements the sp control surface: a small HTTP app that runs on
// one sprite, lists every sprite in the org, and offers the "revival ladder" of
// operations on each — wake the machine, start Claude, hold it Active, hand it
// to Remote Control.
//
// It exists because a laptop's connection dying puts sprites to sleep, and
// there is otherwise no way to revive one from a phone. The hub is reachable at
// the sprite's own URL, which is already behind the Sprites org login, so it is
// private without any auth code of its own.
//
// Two halves:
//
//   - the server (`sp hub serve`), running as a sprite-env service on the hub
//     sprite, which renders the page and performs actions via the sprite CLI;
//   - the agent (`sp hub agent`), an optional sprite-env service on each watched
//     sprite, which reports its own ladder state so the page can show detail for
//     sprites nobody is touching.
//
// The agent matters because of a hard constraint: asking a sprite anything wakes
// it. Without agents, the page can only show what the org listing already knows
// (the machine state) and must wake a sprite to learn whether Claude is alive.
// With agents, each sprite volunteers that while it happens to be awake, and the
// page shows last-known state with an age instead.
package hub

import "time"

// Rung is a step on the revival ladder. Each one can be down while the one
// below it is up, which is why the page shows a rung rather than "up/down".
type Rung int

const (
	// RungCold means the machine is not running. Everything above is
	// necessarily down: a cold restart resets the process namespace.
	RungCold Rung = iota
	// RungMachine means the machine runs but no tmux session was found.
	RungMachine
	// RungTmux means a tmux session exists but no Claude process in it.
	RungTmux
	// RungClaude means Claude is running.
	RungClaude
	// RungRemote means the Remote Control service is registered.
	RungRemote
)

// String names the rung for display.
func (r Rung) String() string {
	switch r {
	case RungCold:
		return "asleep"
	case RungMachine:
		return "awake"
	case RungTmux:
		return "tmux"
	case RungClaude:
		return "claude"
	case RungRemote:
		return "remote control"
	}
	return "unknown"
}

// Session is one resumable Claude conversation found on a sprite, read from the
// JSONL transcripts under ~/.claude/projects. The ID is what `claude --resume`
// takes; Summary is the first real user message, trimmed for a phone screen.
type Session struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"`
	Summary  string    `json:"summary"`
	Modified time.Time `json:"modified"`
}

// Report is what an agent posts about its own sprite. Everything here is
// gathered locally on that sprite, so producing it costs no sprite API calls.
type Report struct {
	Sprite     string    `json:"sprite"`
	TmuxOK     bool      `json:"tmux_ok"`
	ClaudeOK   bool      `json:"claude_ok"`
	ClaudeDir  string    `json:"claude_dir"`
	RemoteOK   bool      `json:"remote_ok"`
	T3OK       bool      `json:"t3_ok"`
	HoldOK     bool      `json:"hold_ok"`
	Sessions   []Session `json:"sessions"`
	ReportedAt time.Time `json:"reported_at"`
}

// Rung reduces a report to the highest ladder step it reached. It assumes the
// machine is up, since a report can only have come from a running machine.
func (r *Report) Rung() Rung {
	switch {
	case r.RemoteOK:
		return RungRemote
	case r.ClaudeOK:
		return RungClaude
	case r.TmuxOK:
		return RungTmux
	}
	return RungMachine
}

// Card is one row on the page: the org listing's view of a sprite merged with
// whatever its agent last reported.
type Card struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"` // running | warm | cold, from the org listing
	URL     string  `json:"url"`
	Report  *Report `json:"report,omitempty"`
	Watched bool    `json:"watched"`
}

// Rung gives the card's ladder position. A cold machine is RungCold regardless
// of what an agent said earlier — a cold restart kills every process, so a stale
// report claiming Claude was alive describes a process that no longer exists.
func (c *Card) Rung() Rung {
	if c.Status != "running" {
		return RungCold
	}
	if c.Report == nil {
		return RungMachine
	}
	return c.Report.Rung()
}

// ReportAge says how long ago the agent last reported, and whether it ever has.
func (c *Card) ReportAge() (time.Duration, bool) {
	if c.Report == nil || c.Report.ReportedAt.IsZero() {
		return 0, false
	}
	return time.Since(c.Report.ReportedAt), true
}
