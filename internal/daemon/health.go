package daemon

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

// HealthMonitor tracks network health and individual sprite status with
// adaptive polling and exponential backoff.
type HealthMonitor struct {
	db     *store.DB
	daemon *Daemon // reference to owning daemon for proxy management and restart
	mu     sync.RWMutex
	online bool // can we reach the sprites API?

	// Per-sprite backoff tracking
	backoffs   map[string]*backoffState
	backoffsMu sync.RWMutex

	// Tracks when sprites entered the "connecting" state for auto-recovery
	connectingSince   map[string]time.Time
	connectingSinceMu sync.RWMutex

	// Callback when state changes
	onUpdate func(StateUpdate)
}

// connectingRecoveryTimeout is how long we wait before attempting recovery
// for a sync session stuck in "connecting" state.
const connectingRecoveryTimeout = 60 * time.Second

// backoffState tracks exponential backoff for individual sprite polling.
type backoffState struct {
	failures  int
	nextPoll  time.Time
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewHealthMonitor creates a new health monitor that tracks sprites and network state.
func NewHealthMonitor(db *store.DB, daemon *Daemon, onUpdate func(StateUpdate)) *HealthMonitor {
	return &HealthMonitor{
		db:              db,
		daemon:          daemon,
		online:          true,
		backoffs:        make(map[string]*backoffState),
		connectingSince: make(map[string]time.Time),
		onUpdate:        onUpdate,
	}
}

// IsOnline returns whether the sprites API is reachable.
func (h *HealthMonitor) IsOnline() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.online
}

// Run starts the health monitoring loop. It checks network connectivity,
// polls sprite health with adaptive intervals, monitors Mutagen sync status,
// and verifies proxy processes are alive.
func (h *HealthMonitor) Run(ctx context.Context) {
	// Fast initial check
	h.checkNetwork()
	h.checkAllSyncStatus()
	h.checkAllProxyLiveness()

	networkTicker := time.NewTicker(15 * time.Second)
	syncTicker := time.NewTicker(10 * time.Second)
	proxyTicker := time.NewTicker(15 * time.Second)
	defer networkTicker.Stop()
	defer syncTicker.Stop()
	defer proxyTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-networkTicker.C:
			h.checkNetwork()
		case <-syncTicker.C:
			if h.IsOnline() {
				h.checkAllSyncStatus()
			}
		case <-proxyTicker.C:
			h.checkAllProxyLiveness()
		}
	}
}

// checkNetwork tests connectivity to the sprites API.
func (h *HealthMonitor) checkNetwork() {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.sprites.dev/v1/sprites")
	wasOnline := h.IsOnline()

	h.mu.Lock()
	if err != nil || resp.StatusCode >= 500 {
		h.online = false
	} else {
		h.online = true
	}
	h.mu.Unlock()

	if resp != nil {
		resp.Body.Close()
	}

	// If we came back online, reset all backoffs
	if !wasOnline && h.IsOnline() {
		h.resetAllBackoffs()
	}
}

// checkAllSyncStatus checks Mutagen sync status for all tracked sprites
// that have sync enabled.
func (h *HealthMonitor) checkAllSyncStatus() {
	sprites, err := h.db.ListSprites(store.ListOptions{})
	if err != nil {
		return
	}

	for _, s := range sprites {
		if s.SyncStatus == "none" || s.SyncStatus == "" {
			continue
		}

		// Check if this sprite is in backoff
		if h.isInBackoff(s.Name) {
			continue
		}

		h.checkSpriteSync(s)
	}
}

// checkSpriteSync checks the sync status of a single sprite and updates the database.
// If sync has been stuck in "connecting" for too long, triggers auto-recovery.
func (h *HealthMonitor) checkSpriteSync(s *store.Sprite) {
	state, err := spSync.GetMutagenStatus(s.Name)
	if err != nil {
		h.recordFailure(s.Name)
		return
	}

	// Reset backoff on success
	h.resetBackoff(s.Name)

	oldSyncStatus := s.SyncStatus
	newSyncStatus := state.Status
	syncError := state.LastError

	// Track how long we've been in "connecting" state
	h.connectingSinceMu.Lock()
	if newSyncStatus == "connecting" {
		if _, exists := h.connectingSince[s.Name]; !exists {
			h.connectingSince[s.Name] = time.Now()
		}
	} else {
		delete(h.connectingSince, s.Name)
	}
	h.connectingSinceMu.Unlock()

	// Auto-recover if stuck in "connecting" too long
	if newSyncStatus == "connecting" {
		h.connectingSinceMu.RLock()
		since, exists := h.connectingSince[s.Name]
		h.connectingSinceMu.RUnlock()
		if exists && time.Since(since) > connectingRecoveryTimeout {
			fmt.Printf("health: sync for %q stuck in connecting for %v, attempting recovery\n",
				s.Name, time.Since(since).Round(time.Second))
			h.connectingSinceMu.Lock()
			delete(h.connectingSince, s.Name)
			h.connectingSinceMu.Unlock()
			if err := h.AttemptSyncRecovery(s.Name); err != nil {
				h.recordFailure(s.Name)
			}
			return
		}
	}

	if oldSyncStatus != newSyncStatus || s.SyncError != syncError {
		if err := h.db.UpdateSyncStatus(s.Name, newSyncStatus, syncError); err != nil {
			return
		}
		if h.onUpdate != nil {
			h.onUpdate(StateUpdate{
				Type:       "sync_status",
				SpriteName: s.Name,
			})
		}
	}
}

// isInBackoff checks if a sprite is currently in an exponential backoff period.
func (h *HealthMonitor) isInBackoff(name string) bool {
	h.backoffsMu.RLock()
	defer h.backoffsMu.RUnlock()
	bs, ok := h.backoffs[name]
	if !ok {
		return false
	}
	return time.Now().Before(bs.nextPoll)
}

// recordFailure increments the failure count and calculates the next poll time
// using exponential backoff.
func (h *HealthMonitor) recordFailure(name string) {
	h.backoffsMu.Lock()
	defer h.backoffsMu.Unlock()

	bs, ok := h.backoffs[name]
	if !ok {
		bs = &backoffState{
			baseDelay: 5 * time.Second,
			maxDelay:  2 * time.Minute,
		}
		h.backoffs[name] = bs
	}

	bs.failures++
	delay := time.Duration(math.Min(
		float64(bs.baseDelay)*math.Pow(2, float64(bs.failures-1)),
		float64(bs.maxDelay),
	))
	bs.nextPoll = time.Now().Add(delay)
}

// resetBackoff clears the backoff state for a sprite.
func (h *HealthMonitor) resetBackoff(name string) {
	h.backoffsMu.Lock()
	defer h.backoffsMu.Unlock()
	delete(h.backoffs, name)
}

// resetAllBackoffs clears all backoff state (e.g., after network recovery).
func (h *HealthMonitor) resetAllBackoffs() {
	h.backoffsMu.Lock()
	defer h.backoffsMu.Unlock()
	h.backoffs = make(map[string]*backoffState)
}

// AttemptSyncRecovery tries to fully recover sync for a sprite by tearing
// down the existing proxy + Mutagen session and re-establishing everything
// from scratch. This is called when a sync session has been in "connecting"
// or "disconnected" state for too long.
func (h *HealthMonitor) AttemptSyncRecovery(spriteName string) error {
	if h.daemon == nil {
		return fmt.Errorf("no daemon reference for sync recovery")
	}

	h.db.UpdateSyncStatus(spriteName, "recovering", "")
	if h.onUpdate != nil {
		h.onUpdate(StateUpdate{Type: "sync_status", SpriteName: spriteName})
	}

	if err := h.daemon.restartSync(spriteName); err != nil {
		h.db.UpdateSyncStatus(spriteName, "error", fmt.Sprintf("recovery failed: %v", err))
		if h.onUpdate != nil {
			h.onUpdate(StateUpdate{Type: "sync_status", SpriteName: spriteName})
		}
		return fmt.Errorf("sync recovery for %q: %w", spriteName, err)
	}

	return nil
}

// checkAllProxyLiveness verifies that proxy processes tracked in the database
// are still alive. If a proxy died but Mutagen thinks sync is "watching", the
// proxy probably exited and sync is silently broken — trigger recovery.
func (h *HealthMonitor) checkAllProxyLiveness() {
	if h.daemon == nil {
		return
	}

	sprites, err := h.db.ListSprites(store.ListOptions{})
	if err != nil {
		return
	}

	for _, s := range sprites {
		if s.SyncStatus == "none" || s.SyncStatus == "" || s.SyncStatus == "disconnected" {
			continue
		}

		if h.isInBackoff(s.Name) {
			continue
		}

		ss, err := h.db.GetSyncSession(s.Name)
		if err != nil || ss == nil {
			continue
		}

		// Check if the proxy PID is still alive
		if ss.ProxyPID > 0 && !isProcessAlive(ss.ProxyPID) {
			// Check if the daemon still tracks this proxy (monitorProxy might
			// have already handled it)
			h.daemon.proxiesMu.RLock()
			_, tracked := h.daemon.proxies[s.Name]
			h.daemon.proxiesMu.RUnlock()

			if !tracked {
				// monitorProxy already handled the death and marked disconnected
				continue
			}

			// Proxy is dead but still tracked — clean up and attempt recovery
			fmt.Printf("health: proxy for %q (pid %d) is dead, attempting recovery\n", s.Name, ss.ProxyPID)
			h.daemon.proxiesMu.Lock()
			delete(h.daemon.proxies, s.Name)
			h.daemon.proxiesMu.Unlock()

			if err := h.AttemptSyncRecovery(s.Name); err != nil {
				h.recordFailure(s.Name)
				fmt.Printf("health: recovery failed for %q: %v\n", s.Name, err)
			}
		}
	}
}

// isProcessAlive checks whether a process with the given PID is still running.
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without sending a real signal
	return proc.Signal(syscall.Signal(0)) == nil
}
