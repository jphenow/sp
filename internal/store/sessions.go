package store

import (
	"database/sql"
	"fmt"
	"time"
)

// UpsertSprite creates or updates a sprite record in the database.
// Fields that are non-empty in the input overwrite existing values.
func (d *DB) UpsertSprite(s *Sprite) error {
	now := time.Now()
	_, err := d.db.Exec(`
		INSERT INTO sprites (name, local_path, remote_path, repo, org, sprite_id, url, status, sync_status, sync_error, last_seen, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			last_seen = excluded.last_seen,
			updated_at = excluded.updated_at
	`, s.Name, s.LocalPath, s.RemotePath, s.Repo, s.Org, s.SpriteID, s.URL,
		s.Status, s.SyncStatus, s.SyncError, now, now, now)
	if err != nil {
		return fmt.Errorf("upserting sprite %q: %w", s.Name, err)
	}
	return nil
}

// GetSprite retrieves a single sprite by name.
func (d *DB) GetSprite(name string) (*Sprite, error) {
	s := &Sprite{}
	err := d.db.QueryRow(`
		SELECT name, local_path, remote_path, repo, org, sprite_id, url,
		       status, sync_status, sync_error, last_seen, created_at, updated_at
		FROM sprites WHERE name = ?
	`, name).Scan(&s.Name, &s.LocalPath, &s.RemotePath, &s.Repo, &s.Org,
		&s.SpriteID, &s.URL, &s.Status, &s.SyncStatus, &s.SyncError,
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
	                 s.status, s.sync_status, s.sync_error, s.last_seen, s.created_at, s.updated_at
	          FROM sprites s`
	var args []interface{}
	var wheres []string

	if len(opts.Tags) > 0 {
		query += ` INNER JOIN tags t ON t.sprite_name = s.name`
		placeholders := ""
		for i, tag := range opts.Tags {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += "?"
			args = append(args, tag)
		}
		wheres = append(wheres, fmt.Sprintf("t.tag IN (%s)", placeholders))
	}

	if opts.PathPrefix != "" {
		wheres = append(wheres, "s.local_path LIKE ?")
		args = append(args, opts.PathPrefix+"%")
	}

	if opts.NameFilter != "" {
		wheres = append(wheres, "s.name LIKE ?")
		args = append(args, "%"+opts.NameFilter+"%")
	}

	for i, w := range wheres {
		if i == 0 {
			query += " WHERE " + w
		} else {
			query += " AND " + w
		}
	}

	query += " ORDER BY s.updated_at DESC"

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing sprites: %w", err)
	}
	defer rows.Close()

	var sprites []*Sprite
	for rows.Next() {
		s := &Sprite{}
		if err := rows.Scan(&s.Name, &s.LocalPath, &s.RemotePath, &s.Repo, &s.Org,
			&s.SpriteID, &s.URL, &s.Status, &s.SyncStatus, &s.SyncError,
			&s.LastSeen, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning sprite row: %w", err)
		}
		sprites = append(sprites, s)
	}
	return sprites, rows.Err()
}

// ListOptions specifies filters for listing sprites.
type ListOptions struct {
	Tags       []string // filter by any of these tags
	PathPrefix string   // filter by local_path prefix
	NameFilter string   // filter by name substring
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
