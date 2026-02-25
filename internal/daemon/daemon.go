package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jphenow/sp/internal/sprite"
	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

// Config holds daemon configuration.
type Config struct {
	SocketPath  string        // Unix socket path
	PIDPath     string        // PID file path
	IdleTimeout time.Duration // Auto-stop after this idle duration (0 = no auto-stop)
}

// binaryCheckInterval is how often we check if the sp binary has been updated.
const binaryCheckInterval = 10 * time.Second

// DefaultConfig returns the default daemon configuration.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".config", "sp")
	return Config{
		SocketPath:  filepath.Join(configDir, "sp.sock"),
		PIDPath:     filepath.Join(configDir, "sp.pid"),
		IdleTimeout: 10 * time.Minute,
	}
}

// Daemon manages sprite state, sync sessions, and serves client requests
// over a Unix socket. Multiple TUI instances and sp processes connect to
// a single daemon.
type Daemon struct {
	config  Config
	db      *store.DB
	client  *sprite.Client
	ln      net.Listener
	mu      sync.RWMutex
	clients map[string]*clientConn // connected client tracking
	cancel  context.CancelFunc
	done    chan struct{}

	// subscribers receive state updates for real-time TUI refresh
	subsMu sync.RWMutex
	subs   map[string]chan StateUpdate

	// proxies tracks running sprite proxy processes owned by the daemon.
	// Key is sprite name, value is the proxy exec.Cmd.
	proxiesMu sync.RWMutex
	proxies   map[string]*exec.Cmd

	// proxyDeathChs is signalled when a tracked proxy exits unexpectedly.
	// handleStartSync watches this to abort early instead of retrying SSH
	// against a dead proxy for 55+ seconds.
	proxyDeathChs   map[string]chan struct{}
	proxyDeathChsMu sync.Mutex

	// startBinaryHash is the SHA-256 of the sp binary at daemon startup.
	// Used to detect when a new binary has been installed.
	startBinaryHash string
	exePath         string // absolute path to the running binary
}

// StateUpdate is broadcast to all connected subscribers when sprite state changes.
type StateUpdate struct {
	Type       string        `json:"type"` // "sprite_status", "sync_status", "sprite_added", "sprite_removed"
	SpriteName string        `json:"sprite_name"`
	Sprite     *store.Sprite `json:"sprite,omitempty"`
}

// clientConn tracks a connected client (TUI or sp process).
type clientConn struct {
	id        string
	conn      net.Conn
	createdAt time.Time
}

// Request is the wire format for client-to-daemon RPC calls.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the wire format for daemon-to-client replies.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// New creates a new daemon instance. Does not start it.
// Computes the hash of the current binary so we can detect updates.
func New(config Config, db *store.DB) *Daemon {
	exePath, _ := os.Executable()
	hash, _ := hashFile(exePath)

	return &Daemon{
		config:          config,
		db:              db,
		client:          sprite.NewClient(""),
		clients:         make(map[string]*clientConn),
		subs:            make(map[string]chan StateUpdate),
		proxies:         make(map[string]*exec.Cmd),
		proxyDeathChs:   make(map[string]chan struct{}),
		done:            make(chan struct{}),
		startBinaryHash: hash,
		exePath:         exePath,
	}
}

// hashFile computes the SHA-256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BinaryHash returns the SHA-256 hex digest of the currently running binary.
// Exported so the TUI and CLI can use the same logic.
func BinaryHash() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return hashFile(exePath)
}

// Start launches the daemon: binds the Unix socket, starts background workers,
// and listens for client connections. Blocks until context is cancelled.
func (d *Daemon) Start(ctx context.Context) error {
	ctx, d.cancel = context.WithCancel(ctx)

	// Write PID file
	if err := d.writePID(); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer d.removePID()

	// Clean up stale socket
	os.Remove(d.config.SocketPath)

	// Ensure socket directory exists
	if err := os.MkdirAll(filepath.Dir(d.config.SocketPath), 0o755); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}

	// Bind Unix socket
	ln, err := net.Listen("unix", d.config.SocketPath)
	if err != nil {
		return fmt.Errorf("binding socket: %w", err)
	}
	d.ln = ln
	defer ln.Close()
	defer os.Remove(d.config.SocketPath)

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start background workers
	go d.healthPoller(ctx)
	go d.idleWatcher(ctx)
	go d.binaryWatcher(ctx)

	// Start the sync/proxy health monitor
	monitor := NewHealthMonitor(d.db, d, d.broadcast)
	go monitor.Run(ctx)

	// Accept loop
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			go d.handleConn(ctx, conn)
		}
	}()

	slog.Info("daemon started", "socket", d.config.SocketPath, "pid", os.Getpid())

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig)
	}

	// Kill all managed proxy processes on shutdown
	d.proxiesMu.RLock()
	names := make([]string, 0, len(d.proxies))
	for name := range d.proxies {
		names = append(names, name)
	}
	d.proxiesMu.RUnlock()
	for _, name := range names {
		d.killProxy(name)
	}

	close(d.done)
	return nil
}

// Stop signals the daemon to shut down.
func (d *Daemon) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

// handleConn processes a single client connection using newline-delimited JSON.
func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	clientID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

	d.mu.Lock()
	d.clients[clientID] = &clientConn{
		id:        clientID,
		conn:      conn,
		createdAt: time.Now(),
	}
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.clients, clientID)
		d.mu.Unlock()
		d.unsubscribe(clientID)
	}()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var req Request
		if err := decoder.Decode(&req); err != nil {
			return // Connection closed or malformed
		}

		resp := d.dispatch(ctx, clientID, &req)
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

// dispatch routes a request to the appropriate handler.
func (d *Daemon) dispatch(ctx context.Context, clientID string, req *Request) Response {
	switch req.Method {
	case "list":
		return d.handleList(req.Params)
	case "get":
		return d.handleGet(req.Params)
	case "upsert":
		return d.handleUpsert(req.Params)
	case "update_status":
		return d.handleUpdateStatus(req.Params)
	case "update_sync_status":
		return d.handleUpdateSyncStatus(req.Params)
	case "delete":
		return d.handleDelete(req.Params)
	case "tag":
		return d.handleTag(req.Params)
	case "untag":
		return d.handleUntag(req.Params)
	case "get_tags":
		return d.handleGetTags(req.Params)
	case "subscribe":
		return d.handleSubscribe(ctx, clientID)
	case "import":
		return d.handleImport(req.Params)
	case "start_sync":
		return d.handleStartSync(req.Params)
	case "stop_sync":
		return d.handleStopSync(req.Params)
	case "restart":
		return d.handleRestart()
	case "ping":
		return respondOK("pong")
	default:
		return respondError(fmt.Sprintf("unknown method: %s", req.Method))
	}
}

// healthPoller periodically checks sprite status from the API and updates the database.
func (d *Daemon) healthPoller(ctx context.Context) {
	// Initial poll on startup
	d.pollSpriteHealth()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pollSpriteHealth()
		}
	}
}

// pollSpriteHealth fetches sprite status from the API and updates the database.
func (d *Daemon) pollSpriteHealth() {
	sprites, err := d.client.List()
	if err != nil {
		// Network issue - don't update status to avoid marking everything unknown
		return
	}

	for _, info := range sprites {
		existing, _ := d.db.GetSprite(info.Name)
		if existing == nil {
			// Don't auto-import sprites we don't know about
			continue
		}

		oldStatus := existing.Status
		changed := false

		// Update status
		if oldStatus != info.Status {
			if err := d.db.UpdateSpriteStatus(info.Name, info.Status); err != nil {
				continue
			}
			changed = true
		}

		// Backfill missing ID, URL, and org from API data
		if (existing.SpriteID == "" && info.ID != "") ||
			(existing.URL == "" && info.URL != "") ||
			(existing.Org == "" && info.Organization != "") {
			patched := *existing
			if patched.SpriteID == "" {
				patched.SpriteID = info.ID
			}
			if patched.URL == "" {
				patched.URL = info.URL
			}
			if patched.Org == "" {
				patched.Org = info.Organization
			}
			patched.Status = info.Status
			d.db.UpsertSprite(&patched)
			changed = true
		}

		if changed {
			d.broadcast(StateUpdate{
				Type:       "sprite_status",
				SpriteName: info.Name,
			})
		}

		// Sync lifecycle management based on status transitions.
		// Only act on sprites that have local+remote paths (i.e., sync was configured).
		syncConfigured := existing.LocalPath != "" && existing.RemotePath != ""
		if !syncConfigured || oldStatus == info.Status {
			continue
		}

		if info.Status == "running" && oldStatus != "running" {
			// Sprite woke up — start sync in the background
			slog.Info("health_poll: sprite woke up, starting sync",
				"sprite", info.Name, "from", oldStatus, "to", info.Status)
			go func(name, org, local, remote string) {
				if err := d.restartSync(name); err != nil {
					slog.Error("health_poll: auto-sync failed", "sprite", name, "error", err)
				}
			}(info.Name, existing.Org, existing.LocalPath, existing.RemotePath)

		} else if info.Status != "running" && oldStatus == "running" {
			// Sprite went to sleep — tear down sync cleanly (not an error)
			slog.Info("health_poll: sprite sleeping, stopping sync",
				"sprite", info.Name, "from", oldStatus, "to", info.Status)
			d.stopSyncForSprite(info.Name)
			d.db.UpdateSyncStatus(info.Name, "idle", "")
			d.broadcast(StateUpdate{Type: "sync_status", SpriteName: info.Name})
		}
	}
}

// idleWatcher monitors for idle state and shuts down the daemon if there are
// no active clients or sync sessions for the configured idle timeout.
func (d *Daemon) idleWatcher(ctx context.Context) {
	if d.config.IdleTimeout == 0 {
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	lastActivity := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.mu.RLock()
			hasClients := len(d.clients) > 0
			d.mu.RUnlock()

			if hasClients {
				lastActivity = time.Now()
				continue
			}

			if time.Since(lastActivity) > d.config.IdleTimeout {
				slog.Info("idle timeout reached, shutting down daemon")
				d.Stop()
				return
			}
		}
	}
}

// binaryWatcher checks periodically whether the sp binary on disk has changed
// since this daemon started. If a new binary is detected, performs a graceful
// restart: cleans up proxies, closes the socket, and re-execs.
func (d *Daemon) binaryWatcher(ctx context.Context) {
	if d.startBinaryHash == "" || d.exePath == "" {
		slog.Warn("binary_watcher: skipping — could not determine binary hash at startup")
		return
	}

	ticker := time.NewTicker(binaryCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentHash, err := hashFile(d.exePath)
			if err != nil {
				slog.Debug("binary_watcher: error hashing binary", "error", err)
				continue
			}
			if currentHash != d.startBinaryHash {
				slog.Info("binary_watcher: new binary detected, restarting daemon",
					"old_hash", d.startBinaryHash[:12], "new_hash", currentHash[:12])
				d.gracefulRestart()
				return
			}
		}
	}
}

// gracefulRestart cleanly shuts down the daemon and re-execs the new binary.
// Proxies are killed (the new daemon will re-establish sync on demand),
// the socket is closed, and we exec into the new binary.
func (d *Daemon) gracefulRestart() {
	slog.Info("graceful_restart: beginning shutdown for re-exec")

	// Kill all managed proxy processes
	d.proxiesMu.RLock()
	names := make([]string, 0, len(d.proxies))
	for name := range d.proxies {
		names = append(names, name)
	}
	d.proxiesMu.RUnlock()
	for _, name := range names {
		d.killProxy(name)
	}

	// Close the listener so the new daemon can bind the socket.
	// Do NOT remove the PID file or socket — the re-exec'd process (same PID)
	// will overwrite them. Removing them creates a race window where
	// EnsureRunning could spawn a duplicate daemon.
	if d.ln != nil {
		d.ln.Close()
	}

	slog.Info("graceful_restart: exec-ing new binary", "path", d.exePath)

	// Re-exec with --reexec flag so the new process skips the IsRunning check
	// (since we're the same PID, IsRunning would say "already running")
	env := append(os.Environ(), "SP_DAEMON_REEXEC=1")
	err := syscall.Exec(d.exePath, []string{d.exePath, "daemon", "start"}, env)
	// If exec fails, we're still running — log and cancel so we stop cleanly
	slog.Error("graceful_restart: exec failed, shutting down", "error", err)
	d.Stop()
}

// subscribe registers a client for state update broadcasts.
func (d *Daemon) subscribe(clientID string) chan StateUpdate {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	ch := make(chan StateUpdate, 100)
	d.subs[clientID] = ch
	return ch
}

// unsubscribe removes a client's state update subscription.
func (d *Daemon) unsubscribe(clientID string) {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	if ch, ok := d.subs[clientID]; ok {
		close(ch)
		delete(d.subs, clientID)
	}
}

// broadcast sends a state update to all subscribers.
func (d *Daemon) broadcast(update StateUpdate) {
	d.subsMu.RLock()
	defer d.subsMu.RUnlock()
	for _, ch := range d.subs {
		select {
		case ch <- update:
		default:
			// Drop if subscriber is slow
		}
	}
}

// writePID writes the current process ID to the PID file.
func (d *Daemon) writePID() error {
	if err := os.MkdirAll(filepath.Dir(d.config.PIDPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(d.config.PIDPath, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// removePID removes the PID file.
func (d *Daemon) removePID() {
	os.Remove(d.config.PIDPath)
}

// IsRunning checks if a daemon is already running by verifying the PID file.
func IsRunning(config Config) bool {
	data, err := os.ReadFile(config.PIDPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	// Check if process exists
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if process exists without actually sending a signal
	return proc.Signal(syscall.Signal(0)) == nil
}

// EnsureRunning starts the daemon as a background process if it's not already running.
// Returns the socket path to connect to.
func EnsureRunning() (string, error) {
	config := DefaultConfig()
	if IsRunning(config) {
		return config.SocketPath, nil
	}

	// Start daemon in background
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("getting executable path: %w", err)
	}

	// Open log file for daemon stdout/stderr so output isn't lost when backgrounded
	logPath := filepath.Join(filepath.Dir(config.SocketPath), "sp.log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logFile = os.Stderr // fallback
	}

	// Fork ourselves with the daemon subcommand
	attr := &os.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, logFile, logFile},
	}

	proc, err := os.StartProcess(exe, []string{exe, "daemon", "start"}, attr)
	if err != nil {
		if logFile != os.Stderr {
			logFile.Close()
		}
		return "", fmt.Errorf("starting daemon: %w", err)
	}
	proc.Release()
	if logFile != os.Stderr {
		logFile.Close() // daemon has its own handle now
	}

	// Wait for socket to appear
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(config.SocketPath); err == nil {
			return config.SocketPath, nil
		}
	}

	return "", fmt.Errorf("daemon did not start within 5 seconds")
}

// --- Request handlers ---

func (d *Daemon) handleList(params json.RawMessage) Response {
	var opts store.ListOptions
	if params != nil {
		if err := json.Unmarshal(params, &opts); err != nil {
			return respondError(fmt.Sprintf("invalid params: %v", err))
		}
	}
	sprites, err := d.db.ListSprites(opts)
	if err != nil {
		return respondError(err.Error())
	}
	return respondJSON(sprites)
}

func (d *Daemon) handleGet(params json.RawMessage) Response {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	s, err := d.db.GetSprite(req.Name)
	if err != nil {
		return respondError(err.Error())
	}
	if s == nil {
		return respondError("sprite not found")
	}
	return respondJSON(s)
}

func (d *Daemon) handleUpsert(params json.RawMessage) Response {
	var s store.Sprite
	if err := json.Unmarshal(params, &s); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	if err := d.db.UpsertSprite(&s); err != nil {
		return respondError(err.Error())
	}
	d.broadcast(StateUpdate{Type: "sprite_added", SpriteName: s.Name})

	// Auto-start sync if the sprite is running and has sync paths configured,
	// and sync isn't already active. The daemon owns the sync lifecycle — callers
	// just register the sprite and the daemon takes it from there.
	syncNeeded := s.LocalPath != "" && s.RemotePath != "" &&
		s.Status == "running" &&
		s.SyncStatus != "watching" && s.SyncStatus != "connecting"
	if syncNeeded {
		// Also check if there's already a proxy tracked (sync in progress)
		d.proxiesMu.RLock()
		_, hasProxy := d.proxies[s.Name]
		d.proxiesMu.RUnlock()

		if !hasProxy {
			slog.Info("upsert: auto-starting sync for running sprite",
				"sprite", s.Name, "local", s.LocalPath, "remote", s.RemotePath)
			go func(name string) {
				if err := d.restartSync(name); err != nil {
					slog.Error("upsert: auto-sync failed", "sprite", name, "error", err)
				}
			}(s.Name)
		}
	}

	return respondOK("ok")
}

func (d *Daemon) handleUpdateStatus(params json.RawMessage) Response {
	var req struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	if err := d.db.UpdateSpriteStatus(req.Name, req.Status); err != nil {
		return respondError(err.Error())
	}
	d.broadcast(StateUpdate{Type: "sprite_status", SpriteName: req.Name})
	return respondOK("ok")
}

func (d *Daemon) handleUpdateSyncStatus(params json.RawMessage) Response {
	var req struct {
		Name       string `json:"name"`
		SyncStatus string `json:"sync_status"`
		SyncError  string `json:"sync_error"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	if err := d.db.UpdateSyncStatus(req.Name, req.SyncStatus, req.SyncError); err != nil {
		return respondError(err.Error())
	}
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: req.Name})
	return respondOK("ok")
}

func (d *Daemon) handleDelete(params json.RawMessage) Response {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	if err := d.db.DeleteSprite(req.Name); err != nil {
		return respondError(err.Error())
	}
	d.broadcast(StateUpdate{Type: "sprite_removed", SpriteName: req.Name})
	return respondOK("ok")
}

func (d *Daemon) handleTag(params json.RawMessage) Response {
	var req struct {
		Name string `json:"name"`
		Tag  string `json:"tag"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	if err := d.db.AddTag(req.Name, req.Tag); err != nil {
		return respondError(err.Error())
	}
	return respondOK("ok")
}

func (d *Daemon) handleUntag(params json.RawMessage) Response {
	var req struct {
		Name string `json:"name"`
		Tag  string `json:"tag"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	if err := d.db.RemoveTag(req.Name, req.Tag); err != nil {
		return respondError(err.Error())
	}
	return respondOK("ok")
}

func (d *Daemon) handleGetTags(params json.RawMessage) Response {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}
	tags, err := d.db.GetTags(req.Name)
	if err != nil {
		return respondError(err.Error())
	}
	return respondJSON(tags)
}

func (d *Daemon) handleSubscribe(ctx context.Context, clientID string) Response {
	ch := d.subscribe(clientID)

	// Send updates in a goroutine - this handler returns immediately with success,
	// then the subscription goroutine pushes updates on the same connection
	go func() {
		d.mu.RLock()
		cc, ok := d.clients[clientID]
		d.mu.RUnlock()
		if !ok {
			return
		}

		encoder := json.NewEncoder(cc.conn)
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-ch:
				if !ok {
					return
				}
				if err := encoder.Encode(update); err != nil {
					return
				}
			}
		}
	}()

	return respondOK("subscribed")
}

func (d *Daemon) handleImport(params json.RawMessage) Response {
	var req struct {
		Name      string   `json:"name"`
		LocalPath string   `json:"local_path"`
		Tags      []string `json:"tags"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}

	// Fetch sprite info from API to populate fields
	info, err := d.client.Get(req.Name)
	if err != nil {
		return respondError(fmt.Sprintf("sprite %q not found in API: %v", req.Name, err))
	}

	s := &store.Sprite{
		Name:      info.Name,
		LocalPath: req.LocalPath,
		SpriteID:  info.ID,
		URL:       info.URL,
		Org:       info.Organization,
		Status:    info.Status,
	}

	// Infer remote path and repo from name
	if strings.HasPrefix(info.Name, "gh-") {
		parts := strings.SplitN(strings.TrimPrefix(info.Name, "gh-"), "--", 2)
		if len(parts) == 2 {
			s.Repo = parts[0] + "/" + parts[1]
			s.RemotePath = "/home/sprite/" + parts[1]
		}
	}

	if err := d.db.UpsertSprite(s); err != nil {
		return respondError(err.Error())
	}

	// Apply tags
	for _, tag := range req.Tags {
		if err := d.db.AddTag(req.Name, tag); err != nil {
			return respondError(fmt.Sprintf("adding tag %q: %v", tag, err))
		}
	}

	d.broadcast(StateUpdate{Type: "sprite_added", SpriteName: info.Name})
	return respondJSON(s)
}

// handleRestart sends back an "ok" and then triggers a graceful restart.
// The restart happens asynchronously so the response can be sent first.
func (d *Daemon) handleRestart() Response {
	slog.Info("restart: triggered via RPC")
	go func() {
		// Small delay to let the response get sent
		time.Sleep(100 * time.Millisecond)
		d.gracefulRestart()
	}()
	return respondOK("restarting")
}

// syncSetupResult is the successful result of attemptSyncSetup.
type syncSetupResult struct {
	MutagenID string
	Port      int
	ProxyPID  int
}

// maxSyncRetries is the number of times to retry the full sync pipeline when
// the proxy dies during setup (e.g., sprite cycling warm/cold).
const maxSyncRetries = 3

// handleStartSync sets up SSH, starts a proxy as a daemon child process, and
// creates a Mutagen sync session. Because the proxy is a child of the daemon
// (not the short-lived `sp` CLI), it survives after `sp . --web` returns.
// Retries the full pipeline up to maxSyncRetries times if the proxy dies
// during setup (which happens when the sprite is cycling).
func (d *Daemon) handleStartSync(params json.RawMessage) Response {
	var req struct {
		SpriteName string `json:"sprite_name"`
		LocalPath  string `json:"local_path"`
		RemotePath string `json:"remote_path"`
		Org        string `json:"org"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}

	log := slog.With("sprite", req.SpriteName, "local", req.LocalPath, "remote", req.RemotePath)
	log.Info("start_sync: beginning sync setup")

	client := sprite.NewClient(req.Org)
	mgr := spSync.NewManager(client)

	var lastErr error
	for attempt := 1; attempt <= maxSyncRetries; attempt++ {
		if attempt > 1 {
			log.Info("start_sync: retrying", "attempt", attempt, "prev_error", lastErr)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		result, err := d.attemptSyncSetup(req.SpriteName, req.LocalPath, req.RemotePath, mgr, log)
		if err == nil {
			log.Info("start_sync: complete",
				"mutagen_id", result.MutagenID, "port", result.Port,
				"proxy_pid", result.ProxyPID, "attempts", attempt)
			return respondJSON(map[string]interface{}{
				"mutagen_id": result.MutagenID,
				"ssh_port":   result.Port,
				"proxy_pid":  result.ProxyPID,
			})
		}
		lastErr = err
	}

	log.Error("start_sync: failed after retries", "attempts", maxSyncRetries, "error", lastErr)
	return respondError(fmt.Sprintf("sync setup failed after %d attempts: %v", maxSyncRetries, lastErr))
}

// attemptSyncSetup runs one attempt of the full sync pipeline: wake, SSH,
// proxy, test, Mutagen. If the proxy dies mid-setup the function returns
// quickly so the caller can retry.
func (d *Daemon) attemptSyncSetup(
	spriteName, localPath, remotePath string,
	mgr *spSync.Manager,
	log *slog.Logger,
) (*syncSetupResult, error) {
	// Clean up any prior attempt
	d.stopSyncForSprite(spriteName)

	// Create a death channel BEFORE starting the proxy so monitorProxy
	// can signal us if the proxy dies while we're still setting up
	deathCh := d.makeProxyDeathCh(spriteName)

	// 0. Wake the sprite
	if err := mgr.WakeSprite(spriteName); err != nil {
		return nil, fmt.Errorf("waking sprite: %w", err)
	}

	// 1. Setup SSH server on the sprite
	log.Info("attempt_sync: setting up SSH server")
	if err := mgr.SetupSSHServer(spriteName); err != nil {
		return nil, fmt.Errorf("SSH server setup: %w", err)
	}

	// 2. Start proxy as a child of the daemon process
	log.Info("attempt_sync: starting proxy")
	proxyCmd, port, err := mgr.StartProxy(spriteName)
	if err != nil {
		return nil, fmt.Errorf("starting proxy: %w", err)
	}
	log.Info("attempt_sync: proxy started", "port", port, "pid", proxyCmd.Process.Pid)

	// Track and monitor the proxy
	d.proxiesMu.Lock()
	d.proxies[spriteName] = proxyCmd
	d.proxiesMu.Unlock()
	go d.monitorProxy(spriteName, proxyCmd)

	// 3. Add SSH config
	log.Info("attempt_sync: adding SSH config", "port", port)
	if err := spSync.AddSSHConfig(spriteName, port); err != nil {
		d.killProxy(spriteName)
		return nil, fmt.Errorf("SSH config: %w", err)
	}

	// 4. Test SSH connectivity — aborts early if proxy dies
	log.Info("attempt_sync: testing SSH connection")
	if err := spSync.TestSSHConnection(spriteName, port, deathCh); err != nil {
		log.Warn("attempt_sync: SSH test failed", "error", err)
		d.killProxy(spriteName)
		spSync.RemoveSSHConfig(spriteName)
		return nil, fmt.Errorf("SSH test: %w", err)
	}
	log.Info("attempt_sync: SSH connection verified")

	// 5. Start Mutagen sync session
	log.Info("attempt_sync: creating mutagen session")
	mutagenID, err := mgr.StartMutagenSession(spriteName, localPath, remotePath)
	if err != nil {
		d.killProxy(spriteName)
		spSync.RemoveSSHConfig(spriteName)
		return nil, fmt.Errorf("Mutagen: %w", err)
	}

	// 6. Persist in DB
	d.db.UpsertSyncSession(&store.SyncSession{
		SpriteName: spriteName,
		MutagenID:  mutagenID,
		SSHPort:    port,
		ProxyPID:   proxyCmd.Process.Pid,
	})

	// 7. Mark sync as active
	d.db.UpdateSyncStatus(spriteName, "watching", "")
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: spriteName})

	return &syncSetupResult{
		MutagenID: mutagenID,
		Port:      port,
		ProxyPID:  proxyCmd.Process.Pid,
	}, nil
}

// handleStopSync terminates the Mutagen session, kills the proxy, and cleans
// up SSH config for a sprite.
func (d *Daemon) handleStopSync(params json.RawMessage) Response {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return respondError(fmt.Sprintf("invalid params: %v", err))
	}

	d.stopSyncForSprite(req.Name)
	return respondOK("ok")
}

// stopSyncForSprite tears down all sync infrastructure for a sprite:
// terminates Mutagen, kills the proxy, removes SSH config, cleans up DB.
func (d *Daemon) stopSyncForSprite(spriteName string) {
	slog.Info("stop_sync: tearing down sync", "sprite", spriteName)

	// Terminate Mutagen session (if it exists)
	if err := spSync.TerminateMutagenSession(spriteName); err != nil {
		slog.Debug("stop_sync: mutagen terminate", "sprite", spriteName, "error", err)
	}

	// Kill proxy
	d.killProxy(spriteName)

	// Remove SSH config
	if err := spSync.RemoveSSHConfig(spriteName); err != nil {
		slog.Debug("stop_sync: remove SSH config", "sprite", spriteName, "error", err)
	}

	// Clean up DB
	d.db.DeleteSyncSession(spriteName)
	d.db.UpdateSyncStatus(spriteName, "none", "")
	slog.Info("stop_sync: teardown complete", "sprite", spriteName)
}

// killProxy stops and removes the tracked proxy process for a sprite.
func (d *Daemon) killProxy(spriteName string) {
	d.proxiesMu.Lock()
	cmd, ok := d.proxies[spriteName]
	if ok {
		delete(d.proxies, spriteName)
	}
	d.proxiesMu.Unlock()

	if ok && cmd.Process != nil {
		pid := cmd.Process.Pid
		slog.Info("kill_proxy: sending SIGTERM", "sprite", spriteName, "pid", pid)
		cmd.Process.Signal(syscall.SIGTERM)
		// Give it a moment to exit cleanly, then force-kill
		done := make(chan struct{})
		go func() {
			cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
			slog.Info("kill_proxy: process exited cleanly", "sprite", spriteName, "pid", pid)
		case <-time.After(3 * time.Second):
			slog.Warn("kill_proxy: force-killing after timeout", "sprite", spriteName, "pid", pid)
			cmd.Process.Kill()
		}
	} else if !ok {
		slog.Debug("kill_proxy: no tracked proxy to kill", "sprite", spriteName)
	}
}

// makeProxyDeathCh creates (or resets) the death notification channel for a
// sprite's proxy. handleStartSync calls this before starting the proxy so it
// can select on the channel during SSH test and Mutagen setup.
func (d *Daemon) makeProxyDeathCh(spriteName string) chan struct{} {
	d.proxyDeathChsMu.Lock()
	defer d.proxyDeathChsMu.Unlock()
	ch := make(chan struct{})
	d.proxyDeathChs[spriteName] = ch
	return ch
}

// signalProxyDeath closes the death channel for a sprite, unblocking anyone
// waiting on it. Called by monitorProxy when a proxy exits.
func (d *Daemon) signalProxyDeath(spriteName string) {
	d.proxyDeathChsMu.Lock()
	defer d.proxyDeathChsMu.Unlock()
	if ch, ok := d.proxyDeathChs[spriteName]; ok {
		close(ch)
		delete(d.proxyDeathChs, spriteName)
	}
}

// monitorProxy watches a proxy process and handles its exit. If the sprite
// went warm/cold, this is expected and sync is marked "idle". If the sprite
// is still running, the proxy died unexpectedly and sync is "disconnected".
func (d *Daemon) monitorProxy(spriteName string, cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	slog.Debug("monitor_proxy: watching", "sprite", spriteName, "pid", pid)

	err := cmd.Wait()

	// Check if we still own this proxy (it might have been intentionally killed)
	d.proxiesMu.RLock()
	current, tracked := d.proxies[spriteName]
	d.proxiesMu.RUnlock()

	if !tracked || current != cmd {
		slog.Debug("monitor_proxy: proxy was replaced or stopped intentionally", "sprite", spriteName, "pid", pid)
		return
	}

	// Clean up the tracked proxy
	d.proxiesMu.Lock()
	delete(d.proxies, spriteName)
	d.proxiesMu.Unlock()

	// Signal anyone waiting on this proxy (e.g., handleStartSync during SSH test)
	d.signalProxyDeath(spriteName)

	stderr := sprite.ProxyStderr(cmd)

	// Check if the sprite went to sleep — that's expected, not an error
	info, apiErr := d.client.Get(spriteName)
	if apiErr == nil && info != nil && info.Status != "running" {
		slog.Info("monitor_proxy: proxy exited because sprite is sleeping",
			"sprite", spriteName, "pid", pid, "status", info.Status,
			"stderr", stderr)
		// Clean teardown — sprite is asleep, we'll re-sync when it wakes
		spSync.TerminateMutagenSession(spriteName)
		spSync.RemoveSSHConfig(spriteName)
		d.db.DeleteSyncSession(spriteName)
		d.db.UpdateSyncStatus(spriteName, "idle", "")
		d.broadcast(StateUpdate{Type: "sync_status", SpriteName: spriteName})
		return
	}

	// Sprite is still running but proxy died — unexpected
	errMsg := "proxy exited unexpectedly"
	if err != nil {
		errMsg = fmt.Sprintf("proxy exited: %v", err)
	}

	slog.Error("monitor_proxy: unexpected exit while sprite running",
		"sprite", spriteName, "pid", pid, "error", errMsg, "stderr", stderr)
	d.db.UpdateSyncStatus(spriteName, "disconnected", errMsg)
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: spriteName})
}

// restartSync tears down and re-establishes sync for a sprite using the stored
// session info. Called by the health monitor when recovery is needed.
// Reuses attemptSyncSetup with retry logic.
func (d *Daemon) restartSync(spriteName string) error {
	log := slog.With("sprite", spriteName)
	log.Info("restart_sync: beginning full sync restart")

	// Look up sprite info from the database
	s, err := d.db.GetSprite(spriteName)
	if err != nil || s == nil {
		return fmt.Errorf("sprite %q not found in database", spriteName)
	}

	if s.LocalPath == "" || s.RemotePath == "" {
		return fmt.Errorf("sprite %q has no local/remote path configured", spriteName)
	}

	log = log.With("local", s.LocalPath, "remote", s.RemotePath, "org", s.Org)

	client := sprite.NewClient(s.Org)
	mgr := spSync.NewManager(client)

	var lastErr error
	for attempt := 1; attempt <= maxSyncRetries; attempt++ {
		if attempt > 1 {
			log.Info("restart_sync: retrying", "attempt", attempt, "prev_error", lastErr)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		result, err := d.attemptSyncSetup(spriteName, s.LocalPath, s.RemotePath, mgr, log)
		if err == nil {
			log.Info("restart_sync: complete",
				"mutagen_id", result.MutagenID, "port", result.Port,
				"proxy_pid", result.ProxyPID, "attempts", attempt)
			return nil
		}
		lastErr = err
	}

	log.Error("restart_sync: failed after retries", "attempts", maxSyncRetries, "error", lastErr)
	d.db.UpdateSyncStatus(spriteName, "error", lastErr.Error())
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: spriteName})
	return fmt.Errorf("sync setup failed after %d attempts: %w", maxSyncRetries, lastErr)
}

// --- Response helpers ---

func respondOK(msg string) Response {
	data, _ := json.Marshal(msg)
	return Response{Result: data}
}

func respondError(msg string) Response {
	return Response{Error: msg}
}

func respondJSON(v interface{}) Response {
	data, err := json.Marshal(v)
	if err != nil {
		return respondError(fmt.Sprintf("marshal error: %v", err))
	}
	return Response{Result: data}
}
