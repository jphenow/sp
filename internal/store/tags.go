package store

import (
	"fmt"
)

// AddTag adds a tag to a sprite. No-op if the tag already exists.
func (d *DB) AddTag(spriteName, tag string) error {
	_, err := d.db.Exec(`
		INSERT INTO tags (sprite_name, tag) VALUES (?, ?)
		ON CONFLICT DO NOTHING
	`, spriteName, tag)
	if err != nil {
		return fmt.Errorf("adding tag %q to sprite %q: %w", tag, spriteName, err)
	}
	return nil
}

// RemoveTag removes a tag from a sprite.
func (d *DB) RemoveTag(spriteName, tag string) error {
	_, err := d.db.Exec(`DELETE FROM tags WHERE sprite_name = ? AND tag = ?`, spriteName, tag)
	if err != nil {
		return fmt.Errorf("removing tag %q from sprite %q: %w", tag, spriteName, err)
	}
	return nil
}

// GetTags returns all tags for a sprite.
func (d *DB) GetTags(spriteName string) ([]string, error) {
	rows, err := d.db.Query(`SELECT tag FROM tags WHERE sprite_name = ? ORDER BY tag`, spriteName)
	if err != nil {
		return nil, fmt.Errorf("getting tags for sprite %q: %w", spriteName, err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scanning tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// ListAllTags returns all unique tags across all sprites.
func (d *DB) ListAllTags() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT tag FROM tags ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("listing all tags: %w", err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scanning tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}
