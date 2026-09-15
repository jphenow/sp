package cmd

import (
	"bufio"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jphenow/sp/internal/sprite"
)

// Claude Code's /login opens an authorize URL whose redirect_uri points at a
// callback server it runs on the SPRITE: http://localhost:<random port>/callback.
// The browser runs on this machine, so that redirect only completes if the port
// is forwarded here. When it isn't, Claude falls back to making you copy and
// paste an auth code.
//
// `sprite console` gets this for free: the sprite CLI auto-forwards ports opened
// by descendants of the exec's process. sp can't, because its tmux server
// daemonizes (reparents to PID 1), so claude inside tmux is never a descendant
// of the exec that the CLI is watching — verified on a live sprite.
//
// So sp does the forwarding itself. The image's browser shim
// (/.sprite/bin/sprite-browser, Claude's $BROWSER) already appends every URL it
// opens to /tmp/xdg-open.log as "FOUND_URL: <url>". A background non-TTY exec
// tails that log; when an authorize URL with a localhost redirect appears, sp
// runs `sprite proxy <port>` locally for a few minutes.
//
// Reading the shim's existing log, rather than installing a shim of sp's own,
// means it also works for a claude that was already running in a pane before
// this connect — a new $BROWSER would only reach processes started afterwards.
// If the image ever changes that log format, this silently degrades back to
// the paste-code flow; nothing else depends on it.

// browserOpenLog is where the image's sprite-browser shim records opened URLs.
const browserOpenLog = "/tmp/xdg-open.log"

// loginForwardTTL is how long a callback port stays forwarded. The redirect
// happens seconds after the user approves in the browser; a few minutes covers
// a slow approval without leaving a stray forward around.
const loginForwardTTL = 5 * time.Minute

// callbackPortPattern extracts the port from a localhost redirect_uri, in either
// URL-encoded (as Claude emits it) or plain form.
var callbackPortPattern = regexp.MustCompile(
	`(?i)redirect_uri=http(?:%3A|:)(?:%2F|/)(?:%2F|/)(?:localhost|127\.0\.0\.1)(?:%3A|:)(\d{2,5})`)

// callbackPortFromLogLine returns the OAuth callback port referenced by a line
// of the browser-open log, or 0 if the line isn't a localhost-redirect URL.
func callbackPortFromLogLine(line string) int {
	m := callbackPortPattern.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port < 1024 || port > 65535 {
		return 0
	}
	return port
}

// loginForwarder forwards OAuth callback ports from a sprite for the lifetime
// of one attach.
type loginForwarder struct {
	client     *sprite.Client
	spriteName string
	org        string

	mu      sync.Mutex
	stopped bool
	watcher *exec.Cmd
	proxies map[int]*exec.Cmd
}

// startLoginCallbackForwarder begins watching the sprite for /login callback
// ports and returns a stop function that tears down the watcher and any live
// forwards. Best-effort throughout: any failure just leaves the paste-code flow
// in place, and nothing is printed, because the terminal belongs to the
// attached session.
func startLoginCallbackForwarder(client *sprite.Client, spriteName, org string) (stop func()) {
	f := &loginForwarder{
		client:     client,
		spriteName: spriteName,
		org:        org,
		proxies:    map[int]*exec.Cmd{},
	}

	// tail -n0: only URLs opened from now on. A non-TTY exec is reaped by the
	// sprite ~10s after its connection drops, so killing the local process is
	// enough to clean up the remote side.
	args := client.BuildExecArgs(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     org,
		Command: []string{"sh", "-c", "touch " + browserOpenLog + " 2>/dev/null; exec tail -n0 -F " + browserOpenLog},
	})
	cmd := exec.Command("sprite", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Debug("login forwarder: stdout pipe", "err", err)
		return func() {}
	}
	if err := cmd.Start(); err != nil {
		slog.Debug("login forwarder: starting watcher", "err", err)
		return func() {}
	}
	f.watcher = cmd

	go f.watch(stdout)
	return f.stop
}

// watch reads browser-open log lines and forwards each callback port it finds.
func (f *loginForwarder) watch(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Authorize URLs are long; don't let a big line end the watch.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if port := callbackPortFromLogLine(scanner.Text()); port != 0 {
			f.forward(port)
		}
	}
}

// forward starts `sprite proxy <port>` unless that port is already forwarded,
// and schedules it to be torn down after loginForwardTTL.
func (f *loginForwarder) forward(port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	if _, ok := f.proxies[port]; ok {
		return
	}
	proxy, err := f.client.StartProxy(sprite.ProxyOptions{
		Sprite: f.spriteName,
		Org:    f.org,
		Ports:  []string{strconv.Itoa(port)},
	})
	if err != nil {
		slog.Debug("login forwarder: starting proxy", "port", port, "err", err)
		return
	}
	f.proxies[port] = proxy
	slog.Debug("login forwarder: forwarding callback port", "port", port)

	// Reap the process when it exits on its own (e.g. the port was already
	// taken locally by another sp attached to the same sprite).
	go func() { _ = proxy.Wait() }()

	time.AfterFunc(loginForwardTTL, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.proxies[port] == proxy {
			killProcess(proxy)
			delete(f.proxies, port)
		}
	})
}

// stop tears down the watcher and every live forward. Safe to call once the
// attach has ended; later calls are no-ops.
func (f *loginForwarder) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	f.stopped = true
	killProcess(f.watcher)
	for port, proxy := range f.proxies {
		killProcess(proxy)
		delete(f.proxies, port)
	}
}

// killProcess kills a started command, ignoring one that has already exited.
func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
