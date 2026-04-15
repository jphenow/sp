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
	// Variant is a free-form label appended to the sprite name when spawning
	// a parallel experiment ("sp . scratch-idea"). Empty for regular sprites.
	Variant string
	// BaseName is the sprite name without the variant suffix. Equal to Name
	// when Variant is empty. Indexed for "list all variants of this base" queries.
	BaseName string
	// Pinned marks a variant sprite as graduated from the throwaway pool;
	// `sp prune` skips pinned variants. Meaningless for non-variant sprites.
	Pinned    bool
	LastSeen  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
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

	// Additive column migrations for the variant feature. These run after the
	// base migrations so they work on both fresh and existing databases.
	additions := []struct {
		column string
		ddl    string
	}{
		{"variant", `ALTER TABLE sprites ADD COLUMN variant TEXT DEFAULT ''`},
		{"base_name", `ALTER TABLE sprites ADD COLUMN base_name TEXT DEFAULT ''`},
		{"pinned", `ALTER TABLE sprites ADD COLUMN pinned BOOLEAN DEFAULT 0`},
	}
	for _, a := range additions {
		if err := d.addColumnIfMissing("sprites", a.column, a.ddl); err != nil {
			return fmt.Errorf("adding column %s: %w", a.column, err)
		}
	}
	return nil
}

// addColumnIfMissing runs an ALTER TABLE only if the column doesn't already
// exist. SQLite lacks ADD COLUMN IF NOT EXISTS, so we inspect PRAGMA table_info.
func (d *DB) addColumnIfMissing(table, column, ddl string) error {
	rows, err := d.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = d.db.Exec(ddl)
	return err
}

// SQL returns the underlying *sql.DB for advanced queries. Use sparingly.
func (d *DB) SQL() *sql.DB {
	return d.db
}
