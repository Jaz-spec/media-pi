package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Event trigger_status values.
const (
	TriggerPending       = "pending"
	TriggerFired         = "fired"
	TriggerSkippedManual = "skipped_manual"
	TriggerMissed        = "missed"
	TriggerCancelled     = "cancelled"
)

// Event is a scheduled workshop event cached locally.
type Event struct {
	ID            string
	WorkshopName  string
	StartTime     time.Time
	EndTime       time.Time
	TriggerStatus string
	RecordingID   sql.NullInt64
	LastSeenAt    time.Time
}

// UpsertEvent inserts or updates an event's core fields and bumps last_seen_at.
// trigger_status is not changed on upsert — the scheduler owns transitions.
func (db *DB) UpsertEvent(ctx context.Context, e Event) error {
	now := time.Now().UTC().Unix()
	_, err := db.ExecContext(ctx, `
		INSERT INTO events
		  (id, workshop_name, start_time, end_time, trigger_status, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  workshop_name = excluded.workshop_name,
		  start_time    = excluded.start_time,
		  end_time      = excluded.end_time,
		  last_seen_at  = excluded.last_seen_at,
		  updated_at    = ?`,
		e.ID, e.WorkshopName, e.StartTime.Unix(), e.EndTime.Unix(),
		TriggerPending, now, now,
	)
	return err
}

// UpdateEventTrigger transitions an event's trigger_status and optionally
// links it to a recording row.
func (db *DB) UpdateEventTrigger(ctx context.Context, id, newStatus string, recordingID int64) error {
	var rec any
	if recordingID > 0 {
		rec = recordingID
	}
	_, err := db.ExecContext(ctx, `
		UPDATE events
		SET trigger_status = ?, recording_id = COALESCE(?, recording_id),
		    updated_at = ?
		WHERE id = ?`,
		newStatus, rec, time.Now().UTC().Unix(), id,
	)
	return err
}

// GetEvent returns a single event by id.
func (db *DB) GetEvent(ctx context.Context, id string) (*Event, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, workshop_name, start_time, end_time, trigger_status,
		       recording_id, last_seen_at
		FROM events WHERE id = ?`, id)
	return scanEvent(row)
}

// DueEvents returns events whose start_time has passed, still pending, with
// end_time in the future (skip events whose window has already closed).
func (db *DB) DueEvents(ctx context.Context, now time.Time) ([]Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, workshop_name, start_time, end_time, trigger_status,
		       recording_id, last_seen_at
		FROM events
		WHERE trigger_status = ? AND start_time <= ? AND end_time > ?
		ORDER BY start_time`,
		TriggerPending, now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvents(rows)
}

// UpcomingEvents returns pending events in the window [now, horizon].
// Used by the interlock check and the TUI schedule pane.
func (db *DB) UpcomingEvents(ctx context.Context, now time.Time, horizon time.Duration) ([]Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, workshop_name, start_time, end_time, trigger_status,
		       recording_id, last_seen_at
		FROM events
		WHERE trigger_status = ? AND start_time > ? AND start_time <= ?
		ORDER BY start_time`,
		TriggerPending, now.Unix(), now.Add(horizon).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvents(rows)
}

// ScheduledRecordingsEnding returns rows that the scheduler has already
// fired whose end_time has now passed — caller stops them.
func (db *DB) ScheduledRecordingsEnding(ctx context.Context, now time.Time) ([]Event, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, workshop_name, start_time, end_time, trigger_status,
		       recording_id, last_seen_at
		FROM events
		WHERE trigger_status = ? AND end_time <= ?`,
		TriggerFired, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvents(rows)
}

// CancelStaleEvents marks events as cancelled if they're still pending and
// haven't been seen in the last window seconds. Returns the number cancelled.
func (db *DB) CancelStaleEvents(ctx context.Context, staleWindow time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-staleWindow).Unix()
	res, err := db.ExecContext(ctx, `
		UPDATE events
		SET trigger_status = ?, updated_at = ?
		WHERE trigger_status = ? AND last_seen_at < ?`,
		TriggerCancelled, time.Now().UTC().Unix(), TriggerPending, cutoff,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanEvent(r rowScanner) (*Event, error) {
	var e Event
	var start, end, seen int64
	err := r.Scan(
		&e.ID, &e.WorkshopName, &start, &end, &e.TriggerStatus,
		&e.RecordingID, &seen,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	e.StartTime = time.Unix(start, 0).UTC()
	e.EndTime = time.Unix(end, 0).UTC()
	e.LastSeenAt = time.Unix(seen, 0).UTC()
	return &e, nil
}

func collectEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}
