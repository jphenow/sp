package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UpsertSprite creates or updates a sprite record in the database.
// Fields that are non-empty in the input overwrite existing values.
// Returns an error if the sprite name is empty.
func (d *DB) UpsertSprite(s *Sprite) error {
	if s.Name == "" {
		return fmt.Errorf("cannot upsert sprite with empty name")
	}
	now := time.Now()
	_, err := d.db.Exec(`
		INSERT INTO sprites (name, local_path, remote_path, repo, org, sprite_id, url, status, sync_status, sync_error, variant, base_name, pinned, last_seen, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			local_path = CASE WHEN excluded.local_path != '' THEN excluded.local_path ELSE sprites.local_path END,
			remote_path = CASE WHEN excluded.remote_path != '' THEN excluded.remote_path ELSE sprites.remote_path END,
			repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE sprites.repo END,
			org = CASE WHEN excluded.org != '' THEN excluded.org ELSE sprites.org END,
			sprite_id = CASE WHEN excluded.sprite_id != '' THEN excluded.sprite_id ELSE sprites.sprite_id END,
			url = CASE WHEN excluded.url != '' THEN excluded.url ELSE sprites.url END,
			status = CASE WHEN excluded.status != '' AND excluded.status != 'unknown' THEN excluded.status ELSE sprites.status END,
			sync_status = CASE WHEN excluded.sync_status != '' AND excluded.sync_status != 'none' THEN excluded.sync_status ELSE sprites.sync_status END,
			sync_error = CASE WHEN excluded.sync_error != '' THEN excluded.sync_error ELSE sprites.sync_error END,
			variant = CASE WHEN excluded.variant != '' THEN excluded.variant ELSE sprites.variant END,
			base_name = CASE WHEN excluded.base_name != '' THEN excluded.base_name ELSE sprites.base_name END,
			last_seen = excluded.last_seen,
			updated_at = excluded.updated_at
	`, s.Name, s.LocalPath, s.RemotePath, s.Repo, s.Org, s.SpriteID, s.URL,
		s.Status, s.SyncStatus, s.SyncError, s.Variant, s.BaseName, s.Pinned, now, now, now)
	if err != nil {
		return fmt.Errorf("upserting sprite %q: %w", s.Name, err)
	}
	return nil
}

// SetPinned updates the pinned flag on a sprite. Pinning is the opt-in signal
// that a variant sprite has graduated and should not be swept by `sp prune`.
func (d *DB) SetPinned(name string, pinned bool) error {
	res, err := d.db.Exec(`UPDATE sprites SET pinned = ?, updated_at = ? WHERE name = ?`,
		pinned, time.Now(), name)
	if err != nil {
		return fmt.Errorf("setting pinned on %q: %w", name, err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("sprite %q not found", name)
	}
	return nil
}

// GetSprite retrieves a single sprite by name.
func (d *DB) GetSprite(name string) (*Sprite, error) {
	s := &Sprite{}
	err := d.db.QueryRow(`
		SELECT name, local_path, remote_path, repo, org, sprite_id, url,
		       status, sync_status, sync_error, variant, base_name, pinned,
		       last_seen, created_at, updated_at
		FROM sprites WHERE name = ?
	`, name).Scan(&s.Name, &s.LocalPath, &s.RemotePath, &s.Repo, &s.Org,
		&s.SpriteID, &s.URL, &s.Status, &s.SyncStatus, &s.SyncError,
		&s.Variant, &s.BaseName, &s.Pinned,
		&s.LastSeen, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting sprite %q: %w", name, err)
	}
	return s, nil
}

// ListSprites returns all sprites, optionally filtered by tags and/or path prefix.
func (d *DB) ListSprites(opts ListOptions) ([]*Sprite, error) {
	query := `SELECT s.name, s.local_path, s.remote_path, s.repo, s.org, s.sprite_id, s.url,
	                 s.status, s.sync_status, s.sync_error, s.variant, s.base_name, s.pinned,
	                 s.last_seen, s.created_at, s.updated_at
	          FROM sprites s`
	var args []any
	var wheres []string

	// Always exclude sprites with empty names (can be created by buggy upserts)
	wheres = append(wheres, "s.name != ''")

	if len(opts.Tags) > 0 {
		query += ` INNER JOIN tags t ON t.sprite_name = s.name`
		var ph strings.Builder
		for i, tag := range opts.Tags {
			if i > 0 {
				ph.WriteString(", ")
			}
			ph.WriteString("?")
			args = append(args, tag)
		}
		wheres = append(wheres, fmt.Sprintf("t.tag IN (%s)", ph.String()))
	}

	if opts.PathPrefix != "" {
		wheres = append(wheres, "s.local_path LIKE ?")
		args = append(args, opts.PathPrefix+"%")
	}

	if opts.NameFilter != "" {
		wheres = append(wheres, "s.name LIKE ?")
		args = append(args, "%"+opts.NameFilter+"%")
	}

	if opts.OnlyVariants {
		wheres = append(wheres, "s.variant != ''")
	}

	if opts.VariantsOf != "" {
		wheres = append(wheres, "s.base_name = ?")
		args = append(args, opts.VariantsOf)
	}

	if opts.OnlyUnpinned {
		wheres = append(wheres, "s.pinned = 0")
	}

	if !opts.OlderThan.IsZero() {
		wheres = append(wheres, "s.updated_at < ?")
		args = append(args, opts.OlderThan)
	}

	var qb strings.Builder
	qb.WriteString(query)
	for i, w := range wheres {
		if i == 0 {
			qb.WriteString(" WHERE ")
		} else {
			qb.WriteString(" AND ")
		}
		qb.WriteString(w)
	}
	qb.WriteString(" ORDER BY s.updated_at DESC")

	rows, err := d.db.Query(qb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("listing sprites: %w", err)
	}
	defer rows.Close()

	var sprites []*Sprite
	for rows.Next() {
		s := &Sprite{}
		if err := rows.Scan(&s.Name, &s.LocalPath, &s.RemotePath, &s.Repo, &s.Org,
			&s.SpriteID, &s.URL, &s.Status, &s.SyncStatus, &s.SyncError,
			&s.Variant, &s.BaseName, &s.Pinned,
			&s.LastSeen, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning sprite row: %w", err)
		}
		sprites = append(sprites, s)
	}
	return sprites, rows.Err()
}

// ListOptions specifies filters for listing sprites.
type ListOptions struct {
	Tags         []string  // filter by any of these tags (store.Tag labels, not variants)
	PathPrefix   string    // filter by local_path prefix
	NameFilter   string    // filter by name substring
	OnlyVariants bool      // only return sprites with a non-empty variant
	VariantsOf   string    // only return sprites with this base_name
	OnlyUnpinned bool      // only return sprites where pinned = false
	OlderThan    time.Time // only return sprites whose updated_at is before this time (zero = no filter)
}

// UpdateSpriteStatus updates only the remote status fields of a sprite.
func (d *DB) UpdateSpriteStatus(name, status string) error {
	_, err := d.db.Exec(`
		UPDATE sprites SET status = ?, last_seen = ?, updated_at = ?
		WHERE name = ?
	`, status, time.Now(), time.Now(), name)
	if err != nil {
		return fmt.Errorf("updating sprite status %q: %w", name, err)
	}
	return nil
}

// UpdateSyncStatus updates the sync-related fields of a sprite.
func (d *DB) UpdateSyncStatus(name, syncStatus, syncError string) error {
	_, err := d.db.Exec(`
		UPDATE sprites SET sync_status = ?, sync_error = ?, updated_at = ?
		WHERE name = ?
	`, syncStatus, syncError, time.Now(), name)
	if err != nil {
		return fmt.Errorf("updating sync status %q: %w", name, err)
	}
	return nil
}

// DeleteSprite removes a sprite and all associated data from the database.
func (d *DB) DeleteSprite(name string) error {
	_, err := d.db.Exec(`DELETE FROM sprites WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("deleting sprite %q: %w", name, err)
	}
	return nil
}

// UpsertSyncSession creates or updates a sync session record.
func (d *DB) UpsertSyncSession(ss *SyncSession) error {
	_, err := d.db.Exec(`
		INSERT INTO sync_sessions (sprite_name, mutagen_id, ssh_port, proxy_pid, alpha_connected, beta_connected, conflicts, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sprite_name) DO UPDATE SET
			mutagen_id = excluded.mutagen_id,
			ssh_port = excluded.ssh_port,
			proxy_pid = excluded.proxy_pid,
			alpha_connected = excluded.alpha_connected,
			beta_connected = excluded.beta_connected,
			conflicts = excluded.conflicts,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at
	`, ss.SpriteName, ss.MutagenID, ss.SSHPort, ss.ProxyPID,
		ss.AlphaConnected, ss.BetaConnected, ss.Conflicts, ss.LastError, time.Now())
	if err != nil {
		return fmt.Errorf("upserting sync session for %q: %w", ss.SpriteName, err)
	}
	return nil
}

// GetSyncSession retrieves the sync session for a sprite.
func (d *DB) GetSyncSession(spriteName string) (*SyncSession, error) {
	ss := &SyncSession{}
	err := d.db.QueryRow(`
		SELECT sprite_name, mutagen_id, ssh_port, proxy_pid, alpha_connected, beta_connected, conflicts, last_error, updated_at
		FROM sync_sessions WHERE sprite_name = ?
	`, spriteName).Scan(&ss.SpriteName, &ss.MutagenID, &ss.SSHPort, &ss.ProxyPID,
		&ss.AlphaConnected, &ss.BetaConnected, &ss.Conflicts, &ss.LastError, &ss.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting sync session for %q: %w", spriteName, err)
	}
	return ss, nil
}

// DeleteSyncSession removes the sync session for a sprite.
func (d *DB) DeleteSyncSession(spriteName string) error {
	_, err := d.db.Exec(`DELETE FROM sync_sessions WHERE sprite_name = ?`, spriteName)
	if err != nil {
		return fmt.Errorf("deleting sync session for %q: %w", spriteName, err)
	}
	return nil
}
