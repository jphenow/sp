package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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
func New(config Config, db *store.DB) *Daemon {
	return &Daemon{
		config:  config,
		db:      db,
		client:  sprite.NewClient(""),
		clients: make(map[string]*clientConn),
		subs:    make(map[string]chan StateUpdate),
		proxies: make(map[string]*exec.Cmd),
		done:    make(chan struct{}),
	}
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

	select {
	case <-ctx.Done():
	case sig := <-sigCh:
		fmt.Printf("Received signal %v, shutting down...\n", sig)
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
		if err := d.db.UpdateSpriteStatus(info.Name, info.Status); err != nil {
			continue
		}

		if oldStatus != info.Status {
			d.broadcast(StateUpdate{
				Type:       "sprite_status",
				SpriteName: info.Name,
			})
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
				fmt.Println("Idle timeout reached, shutting down daemon...")
				d.Stop()
				return
			}
		}
	}
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

	// Fork ourselves with the daemon subcommand
	attr := &os.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}

	proc, err := os.StartProcess(exe, []string{exe, "daemon", "start"}, attr)
	if err != nil {
		return "", fmt.Errorf("starting daemon: %w", err)
	}
	proc.Release()

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

// handleStartSync sets up SSH, starts a proxy as a daemon child process, and
// creates a Mutagen sync session. Because the proxy is a child of the daemon
// (not the short-lived `sp` CLI), it survives after `sp . --web` returns.
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

	// Use a sprite client with the correct org for proxy commands
	client := sprite.NewClient(req.Org)
	mgr := spSync.NewManager(client)

	// Stop any existing sync for this sprite first
	d.stopSyncForSprite(req.SpriteName)

	// 1. Setup SSH server on the sprite
	if err := mgr.SetupSSHServer(req.SpriteName); err != nil {
		return respondError(fmt.Sprintf("SSH server setup: %v", err))
	}

	// 2. Start proxy as a child of the daemon process
	proxyCmd, port, err := mgr.StartProxy(req.SpriteName)
	if err != nil {
		return respondError(fmt.Sprintf("starting proxy: %v", err))
	}

	// Track the proxy process so we can monitor and restart it
	d.proxiesMu.Lock()
	d.proxies[req.SpriteName] = proxyCmd
	d.proxiesMu.Unlock()

	// Monitor proxy process in background — if it dies, mark sync as disconnected
	go d.monitorProxy(req.SpriteName, proxyCmd)

	// 3. Add SSH config entry for Mutagen
	if err := spSync.AddSSHConfig(req.SpriteName, port); err != nil {
		d.killProxy(req.SpriteName)
		return respondError(fmt.Sprintf("adding SSH config: %v", err))
	}

	// 4. Test SSH connectivity through the proxy
	if err := spSync.TestSSHConnection(req.SpriteName, port); err != nil {
		d.killProxy(req.SpriteName)
		spSync.RemoveSSHConfig(req.SpriteName)
		return respondError(fmt.Sprintf("SSH connection test: %v", err))
	}

	// 5. Start Mutagen sync session
	mutagenID, err := mgr.StartMutagenSession(req.SpriteName, req.LocalPath, req.RemotePath)
	if err != nil {
		d.killProxy(req.SpriteName)
		spSync.RemoveSSHConfig(req.SpriteName)
		return respondError(fmt.Sprintf("starting Mutagen: %v", err))
	}

	// 6. Persist sync session info in the database
	d.db.UpsertSyncSession(&store.SyncSession{
		SpriteName: req.SpriteName,
		MutagenID:  mutagenID,
		SSHPort:    port,
		ProxyPID:   proxyCmd.Process.Pid,
	})

	// 7. Update sprite sync status
	d.db.UpdateSyncStatus(req.SpriteName, "watching", "")
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: req.SpriteName})

	return respondJSON(map[string]interface{}{
		"mutagen_id": mutagenID,
		"ssh_port":   port,
		"proxy_pid":  proxyCmd.Process.Pid,
	})
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
	// Terminate Mutagen session (if it exists)
	spSync.TerminateMutagenSession(spriteName)

	// Kill proxy
	d.killProxy(spriteName)

	// Remove SSH config
	spSync.RemoveSSHConfig(spriteName)

	// Clean up DB
	d.db.DeleteSyncSession(spriteName)
	d.db.UpdateSyncStatus(spriteName, "none", "")
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
		cmd.Process.Signal(syscall.SIGTERM)
		// Give it a moment to exit cleanly, then force-kill
		done := make(chan struct{})
		go func() {
			cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
		}
	}
}

// monitorProxy watches a proxy process and marks sync as disconnected if it
// exits unexpectedly. This runs as a goroutine for each active proxy.
func (d *Daemon) monitorProxy(spriteName string, cmd *exec.Cmd) {
	err := cmd.Wait()

	// Check if we still own this proxy (it might have been intentionally killed)
	d.proxiesMu.RLock()
	current, tracked := d.proxies[spriteName]
	d.proxiesMu.RUnlock()

	if !tracked || current != cmd {
		return // Proxy was replaced or intentionally stopped
	}

	// Unexpected exit — clean up and mark disconnected
	d.proxiesMu.Lock()
	delete(d.proxies, spriteName)
	d.proxiesMu.Unlock()

	errMsg := "proxy exited unexpectedly"
	if err != nil {
		errMsg = fmt.Sprintf("proxy exited: %v", err)
	}

	d.db.UpdateSyncStatus(spriteName, "disconnected", errMsg)
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: spriteName})
}

// restartSync tears down and re-establishes sync for a sprite using the stored
// session info. Called by the health monitor when recovery is needed.
func (d *Daemon) restartSync(spriteName string) error {
	// Look up sprite info from the database
	s, err := d.db.GetSprite(spriteName)
	if err != nil || s == nil {
		return fmt.Errorf("sprite %q not found in database", spriteName)
	}

	if s.LocalPath == "" || s.RemotePath == "" {
		return fmt.Errorf("sprite %q has no local/remote path configured", spriteName)
	}

	// Stop existing sync
	d.stopSyncForSprite(spriteName)

	// Re-create with the sprite's org
	client := sprite.NewClient(s.Org)
	mgr := spSync.NewManager(client)

	// Setup SSH
	if err := mgr.SetupSSHServer(spriteName); err != nil {
		return fmt.Errorf("SSH setup: %w", err)
	}

	// Start proxy
	proxyCmd, port, err := mgr.StartProxy(spriteName)
	if err != nil {
		return fmt.Errorf("starting proxy: %w", err)
	}

	d.proxiesMu.Lock()
	d.proxies[spriteName] = proxyCmd
	d.proxiesMu.Unlock()
	go d.monitorProxy(spriteName, proxyCmd)

	// SSH config
	if err := spSync.AddSSHConfig(spriteName, port); err != nil {
		d.killProxy(spriteName)
		return fmt.Errorf("SSH config: %w", err)
	}

	// Test connection
	if err := spSync.TestSSHConnection(spriteName, port); err != nil {
		d.killProxy(spriteName)
		spSync.RemoveSSHConfig(spriteName)
		return fmt.Errorf("SSH test: %w", err)
	}

	// Mutagen
	mutagenID, err := mgr.StartMutagenSession(spriteName, s.LocalPath, s.RemotePath)
	if err != nil {
		d.killProxy(spriteName)
		spSync.RemoveSSHConfig(spriteName)
		return fmt.Errorf("Mutagen: %w", err)
	}

	d.db.UpsertSyncSession(&store.SyncSession{
		SpriteName: spriteName,
		MutagenID:  mutagenID,
		SSHPort:    port,
		ProxyPID:   proxyCmd.Process.Pid,
	})
	d.db.UpdateSyncStatus(spriteName, "watching", "")
	d.broadcast(StateUpdate{Type: "sync_status", SpriteName: spriteName})

	return nil
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
