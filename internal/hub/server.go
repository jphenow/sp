package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jphenow/sp/internal/sprite"
)

// Config configures the hub server.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Org is the Sprites organization to list and act on.
	Org string
	// Token, when set, is required as a bearer token on the agent report
	// endpoint. The sprite URL already gates access to org members, so this is
	// a second lock on the same door rather than the only one.
	Token string
	// HoldFor is how long a hold lasts when toggled on from the page.
	HoldFor time.Duration
	// Vault, when non-empty, is a Claude credentials blob the hub can push to
	// a sprite whose login has expired. Off unless explicitly saved.
	Vault []byte
	// Store persists agent reports and the watched set.
	Store *Store
}

// Server is the hub HTTP application.
type Server struct {
	cfg     Config
	actions *Actions
	jobs    *jobQueue

	// mu guards the listing cache below.
	mu          sync.Mutex
	cached      []sprite.Info
	cachedAt    time.Time
	vault       []byte
	vaultEnable bool
}

// listingTTL is how long the org listing is reused. Short enough that a wake
// shows up on the next pull, long enough that repeated taps and a reloading
// phone don't each cost an API call.
const listingTTL = 15 * time.Second

// NewServer builds a hub server.
func NewServer(cfg Config) *Server {
	if cfg.HoldFor <= 0 {
		cfg.HoldFor = time.Hour
	}
	return &Server{
		cfg:         cfg,
		actions:     NewActions(cfg.Org),
		jobs:        newJobQueue(),
		vault:       cfg.Vault,
		vaultEnable: len(cfg.Vault) > 0,
	}
}

// Handler returns the HTTP routes for the hub.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handlePage)
	// Only the agent endpoint carries its own token. Everything else is
	// reached from a browser, where the sprite URL's own org login has already
	// authenticated the caller — a second token there would just be a password
	// the page has no way to supply.
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/report", s.guard(s.handleReport))
	mux.HandleFunc("POST /api/action", s.handleAction)
	mux.HandleFunc("GET /api/job", s.handleJob)
	return mux
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("sp hub listening on %s (org %q)", s.cfg.Addr, s.cfg.Org)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("hub server: %w", err)
	}
	return nil
}

// guard wraps a machine-facing handler with bearer-token authentication
// when a token is set.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// handlePage renders the dashboard.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(pageHTML))
}

// handleState returns the merged card list: the org listing joined with each
// sprite's last agent report.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	cards, err := s.cards()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cards": cards,
		"vault": s.vaultAvailable(),
	})
}

// cards builds the page model from the cached org listing and stored reports.
func (s *Server) cards() ([]Card, error) {
	infos, err := s.listing()
	if err != nil {
		return nil, err
	}
	reports, err := s.cfg.Store.Reports()
	if err != nil {
		return nil, err
	}
	watched, err := s.cfg.Store.Watched()
	if err != nil {
		return nil, err
	}

	cards := make([]Card, 0, len(infos))
	for _, info := range infos {
		card := Card{
			Name:    info.Name,
			Status:  info.Status,
			URL:     info.URL,
			Watched: watched[info.Name],
		}
		// A cold machine has no processes, so an older report describes a
		// world that no longer exists — drop it rather than show it.
		if rep, ok := reports[info.Name]; ok && info.Status == "running" {
			card.Report = rep
		}
		cards = append(cards, card)
	}

	// Watched first, then anything awake, then by name. The point of the page
	// is reviving what you care about, so those sort to the thumb.
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		if a.Watched != b.Watched {
			return a.Watched
		}
		if (a.Status == "running") != (b.Status == "running") {
			return a.Status == "running"
		}
		return a.Name < b.Name
	})
	return cards, nil
}

// listing returns the org sprite listing, reusing a recent result.
func (s *Server) listing() ([]sprite.Info, error) {
	s.mu.Lock()
	if time.Since(s.cachedAt) < listingTTL && s.cached != nil {
		defer s.mu.Unlock()
		return s.cached, nil
	}
	s.mu.Unlock()

	infos, err := s.actions.List()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cached, s.cachedAt = infos, time.Now()
	s.mu.Unlock()
	return infos, nil
}

// invalidateListing forces the next page load to re-read the org listing,
// so a sprite that was just woken stops showing as cold.
func (s *Server) invalidateListing() {
	s.mu.Lock()
	s.cachedAt = time.Time{}
	s.mu.Unlock()
}

// handleReport accepts an agent's report about its own sprite.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var rep Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rep); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if rep.Sprite == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sprite is required"})
		return
	}
	if rep.ReportedAt.IsZero() {
		rep.ReportedAt = time.Now().UTC()
	}
	if err := s.cfg.Store.SaveReport(&rep); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// actionRequest is the body of POST /api/action.
type actionRequest struct {
	Sprite    string `json:"sprite"`
	Action    string `json:"action"`
	SessionID string `json:"session_id"`
	Dir       string `json:"dir"`
	On        bool   `json:"on"`
}

// handleAction starts a ladder operation and returns a job id to poll.
//
// Actions run as background jobs rather than inline because waking a cold
// sprite takes tens of seconds, and a phone browser will not reliably hold a
// request open that long — it backgrounds the tab and the connection dies.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var req actionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Sprite == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sprite is required"})
		return
	}

	// One job per sprite+action at a time, so a double tap on a laggy
	// connection doesn't start two Claudes.
	key := req.Sprite + ":" + req.Action
	id, started := s.jobs.start(key, func() error { return s.runAction(req) })
	status := http.StatusAccepted
	if !started {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"job": id, "started": started})
}

// runAction performs one ladder operation.
func (s *Server) runAction(req actionRequest) error {
	defer s.invalidateListing()

	switch req.Action {
	case "wake":
		return s.actions.Wake(req.Sprite)

	case "claude":
		// Any ladder action implies the machine must be up, and the report we
		// hold describes the state before this ran — drop it so the page shows
		// "no report yet" rather than stale certainty.
		s.cfg.Store.DropReport(req.Sprite)
		return s.actions.StartClaude(StartClaudeOptions{
			Sprite:    req.Sprite,
			Dir:       req.Dir,
			SessionID: req.SessionID,
		})

	case "hold":
		return s.actions.SetHold(req.Sprite, s.cfg.HoldFor, req.On)

	case "remote":
		s.cfg.Store.DropReport(req.Sprite)
		return s.actions.StartRemote(req.Sprite, req.Dir)

	case "t3":
		s.cfg.Store.DropReport(req.Sprite)
		return s.actions.StartT3(req.Sprite, s.cfg.HoldFor)

	case "credentials":
		creds, ok := s.vaultCopy()
		if !ok {
			return fmt.Errorf("the credential vault is disabled on this hub")
		}
		return s.actions.PushCredentials(req.Sprite, creds)

	case "watch":
		return s.cfg.Store.SetWatched(req.Sprite, req.On)

	default:
		return fmt.Errorf("unknown action %q", req.Action)
	}
}

// vaultAvailable reports whether credential push is enabled.
func (s *Server) vaultAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vaultEnable && len(s.vault) > 0
}

// vaultCopy returns the stored credentials, if the vault is enabled.
func (s *Server) vaultCopy() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.vaultEnable || len(s.vault) == 0 {
		return nil, false
	}
	out := make([]byte, len(s.vault))
	copy(out, s.vault)
	return out, true
}

// handleJob reports the status of a background job.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	job, ok := s.jobs.get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// writeJSON sends a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
