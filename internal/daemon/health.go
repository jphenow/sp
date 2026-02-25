package daemon

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

// HealthMonitor tracks network health and individual sprite status with
// adaptive polling and exponential backoff.
type HealthMonitor struct {
	db     *store.DB
	mu     sync.RWMutex
	online bool // can we reach the sprites API?

	// Per-sprite backoff tracking
	backoffs   map[string]*backoffState
	backoffsMu sync.RWMutex

	// Callback when state changes
	onUpdate func(StateUpdate)
}

// backoffState tracks exponential backoff for individual sprite polling.
type backoffState struct {
	failures  int
	nextPoll  time.Time
	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewHealthMonitor creates a new health monitor that tracks sprites and network state.
func NewHealthMonitor(db *store.DB, onUpdate func(StateUpdate)) *HealthMonitor {
	return &HealthMonitor{
		db:       db,
		online:   true,
		backoffs: make(map[string]*backoffState),
		onUpdate: onUpdate,
	}
}

// IsOnline returns whether the sprites API is reachable.
func (h *HealthMonitor) IsOnline() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.online
}

// Run starts the health monitoring loop. It checks network connectivity,
// polls sprite health with adaptive intervals, and monitors Mutagen sync status.
func (h *HealthMonitor) Run(ctx context.Context) {
	// Fast initial check
	h.checkNetwork()
	h.checkAllSyncStatus()

	networkTicker := time.NewTicker(15 * time.Second)
	syncTicker := time.NewTicker(10 * time.Second)
	defer networkTicker.Stop()
	defer syncTicker.Stop()

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

// AttemptSyncRecovery tries to recover a stuck sync session for a sprite.
// This is called when a sync session has been in "connecting" state too long.
func (h *HealthMonitor) AttemptSyncRecovery(spriteName string) error {
	// Check if Mutagen session exists
	if !spSync.MutagenSessionExists(spriteName) {
		return fmt.Errorf("no mutagen session for %q", spriteName)
	}

	// Try terminating and re-establishing
	// (Full recovery would need the sync manager with proxy restart, etc.)
	// For now, just terminate the stuck session so next connect recreates it
	if err := spSync.TerminateMutagenSession(spriteName); err != nil {
		return fmt.Errorf("terminating stuck session: %w", err)
	}

	// Clean up SSH config
	spSync.RemoveSSHConfig(spriteName)

	if err := h.db.UpdateSyncStatus(spriteName, "disconnected", "recovered: session reset"); err != nil {
		return err
	}

	if h.onUpdate != nil {
		h.onUpdate(StateUpdate{
			Type:       "sync_status",
			SpriteName: spriteName,
		})
	}

	return nil
}
