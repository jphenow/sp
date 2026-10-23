package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jphenow/sp/internal/keepalive"
)

// AgentConfig configures the on-sprite reporter.
type AgentConfig struct {
	// HubURL is the hub sprite's base URL (https://<hub>-<org>.sprites.app).
	HubURL string
	// Token is a Sprites org token. Sprite URLs with "sprite" auth accept
	// `Authorization: Bearer <org token>` alongside browser login, which is
	// what lets a machine post here without a browser session.
	Token string
	// Sprite is this sprite's own name, used as the report key.
	Sprite string
	// Interval is how often to report. Reports are cheap (all local) but each
	// one is an HTTPS call out, so this is not a tight loop.
	Interval time.Duration
	// MaxSessions caps how many Claude sessions are reported per sprite.
	MaxSessions int
}

// RunAgent reports this sprite's ladder state to the hub until ctx is done.
//
// It runs as a sprite-env service, which means it restarts on boot and after a
// cold wake — so a revived sprite starts reporting again without anyone asking
// it to. Report failures are logged and retried on the next tick rather than
// being fatal: the hub being briefly unreachable is not a reason for the agent
// to exit and stay gone until the next wake.
func RunAgent(ctx context.Context, cfg AgentConfig) error {
	if cfg.HubURL == "" {
		return fmt.Errorf("hub URL is required")
	}
	if cfg.Sprite == "" {
		return fmt.Errorf("sprite name is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 8
	}

	client := &http.Client{Timeout: 20 * time.Second}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		report := GatherReport(cfg.Sprite, cfg.MaxSessions)
		if err := postReport(ctx, client, cfg, report); err != nil {
			fmt.Fprintf(os.Stderr, "sp hub agent: report failed: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// postReport sends one report to the hub.
func postReport(ctx context.Context, client *http.Client, cfg AgentConfig, r *Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}
	url := strings.TrimRight(cfg.HubURL, "/") + "/api/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("hub returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// GatherReport inspects the local sprite and builds a report. Everything it
// reads is on this machine, so it costs no sprite API calls and cannot wake
// anything — which is the reason the agent exists at all.
func GatherReport(sprite string, maxSessions int) *Report {
	claudeRunning, claudeDir, remoteRunning := scanClaudeProcesses()
	return &Report{
		Sprite:     sprite,
		TmuxOK:     tmuxAlive(),
		ClaudeOK:   claudeRunning,
		ClaudeDir:  claudeDir,
		RemoteOK:   remoteRunning,
		T3OK:       t3Alive(),
		HoldOK:     holdActive(),
		Sessions:   ClaudeSessions(maxSessions),
		ReportedAt: time.Now().UTC(),
	}
}

// t3Alive reports whether a T3 process is running and so the sprite is
// reachable from the phone.
//
// The service's desired state is not a useful signal here: it says the service
// should be running, not that the relay was actually joined, and the failure
// this is meant to catch is exactly a service that came up and could not
// connect. So it looks for the process.
func t3Alive() bool {
	found := false
	forEachProcess(func(pid string, args []string) {
		if found || len(args) < 2 {
			return
		}
		if !containsArg(args, "serve") && !containsArg(args, "connect") {
			return
		}
		// T3 runs through npx, so argv[0] is the node interpreter and the
		// script path is what identifies it.
		for _, a := range args {
			if filepath.Base(a) == "t3" || strings.Contains(a, "/t3/") {
				found = true
				return
			}
		}
	})
	return found
}

// tmuxAlive reports whether any tmux session exists on this sprite.
func tmuxAlive() bool {
	out, err := exec.Command("tmux", "list-sessions").Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

// holdActive reports whether a keepalive heartbeat currently owns the hold,
// by the presence of the generation file sp's heartbeat maintains.
func holdActive() bool {
	_, err := os.Stat(keepalive.GenFile)
	return err == nil
}

// scanClaudeProcesses walks /proc for Claude processes, returning whether one
// is running, the working directory of the first one found, and whether any of
// them is a Remote Control server.
//
// /proc is read directly rather than shelling out to ps: the format is stable,
// it needs no parsing of column layouts, and the cwd symlink gives the working
// directory, which ps does not report at all.
func scanClaudeProcesses() (running bool, dir string, remote bool) {
	forEachProcess(func(pid string, args []string) {
		if !isClaudeProcess(args) {
			return
		}
		running = true
		if containsArg(args, "remote-control") {
			remote = true
		}
		if dir == "" {
			if cwd, err := os.Readlink(filepath.Join("/proc", pid, "cwd")); err == nil {
				dir = cwd
			}
		}
	})
	return running, dir, remote
}

// forEachProcess calls fn with the pid and argv of every process on this
// sprite except the agent itself.
func forEachProcess(fn func(pid string, args []string)) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := fmt.Sprint(os.Getpid())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if pid == "" || pid[0] < '0' || pid[0] > '9' || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		fn(pid, strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"))
	}
}

// isClaudeProcess reports whether a command line is the Claude CLI. It matches
// on the executable's base name so that a path like
// /home/sprite/.local/bin/claude counts while an unrelated process that merely
// mentions "claude" in an argument does not.
func isClaudeProcess(args []string) bool {
	if len(args) == 0 {
		return false
	}
	base := filepath.Base(args[0])
	if base == "claude" {
		return true
	}
	// Claude ships as a node script in some installs, where argv[0] is the
	// interpreter and argv[1] is the script path.
	if (base == "node" || base == "bun") && len(args) > 1 && filepath.Base(args[1]) == "claude" {
		return true
	}
	return false
}

// containsArg reports whether want appears among the arguments.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// ClaudeSessions lists the most recently modified resumable Claude sessions on
// this sprite, newest first, capped at max.
//
// Reporting these is what makes `--resume` usable from a phone: without them
// the hub would have to wake a sprite just to find out what could be resumed,
// which costs most of what resuming would have cost anyway.
func ClaudeSessions(max int) []Session {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	projects := filepath.Join(home, ".claude", "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}

	var sessions []Session
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(projects, d.Name(), f.Name())
			dir, summary := readSessionHead(path)
			sessions = append(sessions, Session{
				ID:       strings.TrimSuffix(f.Name(), ".jsonl"),
				Dir:      dir,
				Summary:  summary,
				Modified: info.ModTime().UTC(),
			})
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Modified.After(sessions[j].Modified)
	})
	if len(sessions) > max {
		sessions = sessions[:max]
	}
	return sessions
}

// sessionHeadLimit caps how much of a transcript is read looking for the
// working directory and opening message. Transcripts grow to megabytes and the
// fields we want are in the first few records.
const sessionHeadLimit = 128 << 10

// readSessionHead pulls the working directory and a one-line summary out of the
// start of a session transcript.
//
// The summary is the first real user message. Transcripts open with metadata
// records and with command caveats flagged `isMeta`, none of which describe
// what the session was about, so those are skipped.
func readSessionHead(path string) (dir, summary string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(io.LimitReader(f, sessionHeadLimit))
	scanner.Buffer(make([]byte, 0, 64<<10), sessionHeadLimit)
	for scanner.Scan() {
		var rec struct {
			Type    string `json:"type"`
			IsMeta  bool   `json:"isMeta"`
			Cwd     string `json:"cwd"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if dir == "" && rec.Cwd != "" {
			dir = rec.Cwd
		}
		if summary == "" && rec.Type == "user" && !rec.IsMeta {
			summary = firstText(rec.Message.Content)
		}
		if dir != "" && summary != "" {
			break
		}
	}
	return dir, trimSummary(summary)
}

// firstText extracts display text from a message content field, which is
// either a plain string or an array of typed blocks.
func firstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return b.Text
		}
	}
	return ""
}

// summaryLimit is how much of the opening message to keep, sized for one or
// two lines on a phone.
const summaryLimit = 120

// trimSummary collapses whitespace and truncates for display.
func trimSummary(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) > summaryLimit {
		s = strings.TrimSpace(s[:summaryLimit]) + "…"
	}
	return s
}
