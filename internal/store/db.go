package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps a SQLite database connection for sp state persistence.
type DB struct {
	db *sql.DB
}

// Sprite represents a tracked sprite environment and its current state.
type Sprite struct {
	Name       string
	LocalPath  string
	RemotePath string
	Repo       string
	Org        string
	SpriteID   string
	URL        string
	Status     string // running, warm, cold, unknown
	SyncStatus string // syncing, watching, error, disconnected, none
	SyncError  string
	LastSeen   time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SyncSession tracks the state of a Mutagen sync session for a sprite.
type SyncSession struct {
	SpriteName     string
	MutagenID      string
	SSHPort        int
	ProxyPID       int
	AlphaConnected bool
	BetaConnected  bool
	Conflicts      int
	LastError      string
	UpdatedAt      time.Time
}

// Tag represents a user-assigned label on a sprite for filtering.
type Tag struct {
	SpriteName string
	Tag        string
}

// defaultDBPath returns the default path for the sp database file.
func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("getting home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "sp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating config directory: %w", err)
	}
	return filepath.Join(dir, "sp.db"), nil
}

// Open opens the SQLite database at the default path, creating it if needed,
// and runs any pending migrations.
func Open() (*DB, error) {
	path, err := defaultDBPath()
	if err != nil {
		return nil, err
	}
	return OpenPath(path)
}

// OpenPath opens a SQLite database at the given path and runs migrations.
func OpenPath(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// Enable WAL mode for concurrent readers
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}
	// Enable foreign key constraints (required for ON DELETE CASCADE)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	store := &DB{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return store, nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// migrate runs all schema migrations in order.
func (d *DB) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS sprites (
			name TEXT PRIMARY KEY,
			local_path TEXT,
			remote_path TEXT,
			repo TEXT,
			org TEXT,
			sprite_id TEXT,
			url TEXT,
			status TEXT DEFAULT 'unknown',
			sync_status TEXT DEFAULT 'none',
			sync_error TEXT DEFAULT '',
			last_seen DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS tags (
			sprite_name TEXT REFERENCES sprites(name) ON DELETE CASCADE,
			tag TEXT,
			PRIMARY KEY (sprite_name, tag)
		)`,
		`CREATE TABLE IF NOT EXISTS sync_sessions (
			sprite_name TEXT REFERENCES sprites(name) ON DELETE CASCADE UNIQUE,
			mutagen_id TEXT,
			ssh_port INTEGER,
			proxy_pid INTEGER,
			alpha_connected BOOLEAN DEFAULT 0,
			beta_connected BOOLEAN DEFAULT 0,
			conflicts INTEGER DEFAULT 0,
			last_error TEXT DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, m := range migrations {
		if _, err := d.db.Exec(m); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	return nil
}

// SQL returns the underlying *sql.DB for advanced queries. Use sparingly.
func (d *DB) SQL() *sql.DB {
	return d.db
}
