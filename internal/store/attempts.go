package store

import (
	"fmt"
	"time"
)

// Attempt is one connection attempt to a sprite's interactive session, as it
// ended. Every attempt is recorded, including the one that ends the session
// cleanly: without the clean ones there is no denominator, and "three drops"
// means nothing until you know whether it was over three hours or three days.
type Attempt struct {
	At        time.Time
	Sprite    string
	Transport string // the client that carried it: "sprite" or "ssh"
	// Outcome is one of:
	//   clean   — the session ended on its own, which is the normal exit
	//   dropped — the connection failed and the client said so
	//   stall   — the connection stopped answering input and sp killed it
	//   ended   — the client exited non-zero for some other reason
	Outcome string
	// Uptime is how long this attempt lasted.
	Uptime time.Duration
	// FirstOutput is how long the attempt took to print anything, or 0 when it
	// never did. This is the measurement that makes a paused sprite visible:
	// reattaching to a sprite that is awake paints the tmux session at once,
	// while one that has to be woken first takes seconds to say anything.
	FirstOutput time.Duration
	// Attempt counts from 1 within a single run of sp, so a row with a high
	// number is part of a reconnect run rather than a fresh session.
	Attempt int
	// Detail is the last line the client printed, which for a failure is
	// usually the reason.
	Detail string
}

// RecordAttempt appends one ended attempt to the log.
func (d *DB) RecordAttempt(a Attempt) error {
	if a.At.IsZero() {
		a.At = time.Now()
	}
	// UTC strips the monotonic reading, which the driver would otherwise store
	// as part of the timestamp ("... m=+0.07") and then fail to compare or
	// read back.
	a.At = a.At.UTC()
	_, err := d.db.Exec(`
		INSERT INTO session_attempts
			(at, sprite, transport, outcome, uptime_ms, first_output_ms, attempt, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, a.At, a.Sprite, a.Transport, a.Outcome,
		a.Uptime.Milliseconds(), a.FirstOutput.Milliseconds(), a.Attempt, a.Detail)
	if err != nil {
		return fmt.Errorf("recording session attempt: %w", err)
	}
	return nil
}

// Attempts returns the attempts recorded since a point in time, newest first.
// An empty sprite name returns every sprite's.
func (d *DB) Attempts(since time.Time, sprite string) ([]Attempt, error) {
	query := `SELECT at, sprite, transport, outcome, uptime_ms, first_output_ms, attempt, detail
		FROM session_attempts WHERE at >= ?`
	args := []any{since.UTC()}
	if sprite != "" {
		query += ` AND sprite = ?`
		args = append(args, sprite)
	}
	query += ` ORDER BY at DESC`

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing session attempts: %w", err)
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		var (
			a                 Attempt
			uptimeMS, firstMS int64
		)
		if err := rows.Scan(&a.At, &a.Sprite, &a.Transport, &a.Outcome,
			&uptimeMS, &firstMS, &a.Attempt, &a.Detail); err != nil {
			return nil, fmt.Errorf("scanning session attempt: %w", err)
		}
		a.Uptime = time.Duration(uptimeMS) * time.Millisecond
		a.FirstOutput = time.Duration(firstMS) * time.Millisecond
		out = append(out, a)
	}
	return out, rows.Err()
}
