package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists agent reports and the watched set for the hub.
//
// It is deliberately separate from sp's local sprite database. That one is the
// laptop's view of sprites it created and syncs; this one is the hub's view of
// what every sprite in the org is doing. They answer different questions, and
// the hub has to work when the laptop is off, which is the whole point.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the hub database at path and applies
// migrations. An empty path uses the default under the user's config dir.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		var err error
		path, err = DefaultDBPath()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating hub db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening hub db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DefaultDBPath returns the standard location of the hub database.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".config", "sp", "hub.db"), nil
}

// migrate creates the tables the hub needs.
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS reports (
			sprite TEXT PRIMARY KEY,
			body TEXT NOT NULL,
			reported_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS watched (
			sprite TEXT PRIMARY KEY,
			added_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("hub migration failed: %w", err)
		}
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveReport stores an agent's report, replacing any earlier one for that
// sprite. Only the latest matters: the page shows current state, not history.
func (s *Store) SaveReport(r *Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding report: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO reports (sprite, body, reported_at) VALUES (?, ?, ?)
		 ON CONFLICT(sprite) DO UPDATE SET body = excluded.body, reported_at = excluded.reported_at`,
		r.Sprite, string(body), r.ReportedAt,
	)
	if err != nil {
		return fmt.Errorf("saving report for %q: %w", r.Sprite, err)
	}
	return nil
}

// Reports returns the latest report per sprite, keyed by sprite name.
func (s *Store) Reports() (map[string]*Report, error) {
	rows, err := s.db.Query(`SELECT sprite, body FROM reports`)
	if err != nil {
		return nil, fmt.Errorf("loading reports: %w", err)
	}
	defer rows.Close()

	out := map[string]*Report{}
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			return nil, fmt.Errorf("scanning report: %w", err)
		}
		var r Report
		// A report that won't decode is a schema change, not a reason to blank
		// the whole page — skip it and let the card fall back to listing state.
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			continue
		}
		out[name] = &r
	}
	return out, rows.Err()
}

// DropReport forgets a sprite's report. Used when an action invalidates what
// the agent last said (restarting Claude, say) so the page doesn't show state
// that is known to be wrong until the next report arrives.
func (s *Store) DropReport(sprite string) error {
	_, err := s.db.Exec(`DELETE FROM reports WHERE sprite = ?`, sprite)
	if err != nil {
		return fmt.Errorf("dropping report for %q: %w", sprite, err)
	}
	return nil
}

// SetWatched adds or removes a sprite from the watched set. Watched sprites are
// the ones the hub will install an agent on and keep pinned to the top of the
// page; everything else is still listed and still actionable.
func (s *Store) SetWatched(sprite string, watched bool) error {
	var err error
	if watched {
		_, err = s.db.Exec(
			`INSERT INTO watched (sprite, added_at) VALUES (?, ?) ON CONFLICT(sprite) DO NOTHING`,
			sprite, time.Now(),
		)
	} else {
		_, err = s.db.Exec(`DELETE FROM watched WHERE sprite = ?`, sprite)
	}
	if err != nil {
		return fmt.Errorf("updating watched set for %q: %w", sprite, err)
	}
	return nil
}

// Watched returns the set of watched sprite names.
func (s *Store) Watched() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT sprite FROM watched`)
	if err != nil {
		return nil, fmt.Errorf("loading watched set: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning watched row: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}
