package store

import (
	"fmt"
	"time"
)

// Event is one thing sp wants to be able to look at later.
//
// sp's warnings are printed into a session that then takes over the terminal,
// so by the time something goes wrong the explanation has scrolled past or
// been erased with the alternate screen. The ones worth investigating are
// exactly the ones that happen while the user is busy starting work, which is
// the worst moment to ask them to read carefully. Keeping them means a vague
// "I keep seeing weird Claude file errors" can be answered from a record
// instead of from memory.
type Event struct {
	At time.Time
	// Level is "warn" or "error". Anything sp prints as a warning is a warn;
	// errors are reserved for a failure that ended what sp was doing.
	Level string
	// Source names the part of sp that reported it ("claude-config", "sync",
	// "hold"), so a recurring problem can be counted by area.
	Source string
	// Sprite is the sprite in play, when there is one.
	Sprite string
	// Message is the human-readable line, as printed.
	Message string
	// Detail carries whatever would not fit in the message: an underlying
	// error, a command's output.
	Detail string
}

// RecordEvent appends one event to the log.
func (d *DB) RecordEvent(e Event) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	// UTC strips the monotonic reading the driver would otherwise store as
	// part of the timestamp. See RecordAttempt.
	e.At = e.At.UTC()
	if e.Level == "" {
		e.Level = "warn"
	}
	_, err := d.db.Exec(`
		INSERT INTO events (at, level, source, sprite, message, detail)
		VALUES (?, ?, ?, ?, ?, ?)
	`, e.At, e.Level, e.Source, e.Sprite, e.Message, e.Detail)
	if err != nil {
		return fmt.Errorf("recording event: %w", err)
	}
	return nil
}

// EventFilter narrows a read of the log. A zero value reads everything since
// the given time.
type EventFilter struct {
	Sprite string
	Level  string
	Source string
	// Limit caps how many rows come back, newest first. 0 means no cap.
	Limit int
}

// Events returns the events recorded since a point in time, newest first.
func (d *DB) Events(since time.Time, f EventFilter) ([]Event, error) {
	query := `SELECT at, level, source, sprite, message, detail FROM events WHERE at >= ?`
	args := []any{since.UTC()}
	for _, c := range []struct {
		column string
		value  string
	}{
		{"sprite", f.Sprite},
		{"level", f.Level},
		{"source", f.Source},
	} {
		if c.value != "" {
			query += ` AND ` + c.column + ` = ?`
			args = append(args, c.value)
		}
	}
	query += ` ORDER BY at DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.At, &e.Level, &e.Source, &e.Sprite, &e.Message, &e.Detail); err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes events and session attempts older than the cutoff, returning
// how many rows went.
//
// Both tables only ever grow, and what makes them useful — a record of every
// warning and every attempt — is also what makes them unbounded. Old rows are
// not worth keeping: a warning from two months ago cannot be investigated any
// more, since the sprite, the session and usually the code have all moved on.
func (d *DB) Prune(before time.Time) (int64, error) {
	before = before.UTC()
	var total int64
	for _, table := range []string{"events", "session_attempts"} {
		res, err := d.db.Exec(`DELETE FROM `+table+` WHERE at < ?`, before)
		if err != nil {
			return total, fmt.Errorf("pruning %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("pruning %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}
