package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Recording status values.
const (
	RecordingRecording = "recording"
	RecordingStopped   = "stopped"
	RecordingFailed    = "failed"
)

// Stop reason values.
const (
	StopReasonManual           = "manual"
	StopReasonScheduledEnd     = "scheduled_end"
	StopReasonFFmpegDied       = "ffmpeg_died"
	StopReasonOperatorOverride = "operator_override"
)

// Recording is the in-Go view of a recordings row.
type Recording struct {
	ID            int64
	EventID       sql.NullString
	FilePath      string
	Status        string
	StartedAt     time.Time
	StoppedAt     sql.NullTime
	StopReason    sql.NullString
	FFmpegPID     sql.NullInt64
	FFmpegLogPath sql.NullString
	Notes         sql.NullString
}

// ErrNoActiveRecording is returned when there's no active recording to stop.
var ErrNoActiveRecording = errors.New("no active recording")

// NewRecordingInput is the data needed to open a recording row.
type NewRecordingInput struct {
	EventID       string // optional; empty = manual recording
	FilePath      string
	FFmpegPID     int64
	FFmpegLogPath string
}

// StartRecording inserts a new row with status='recording'. Returns the id.
// Fails if there's already an active recording — only one row can be
// 'recording' at a time.
func (db *DB) StartRecording(ctx context.Context, in NewRecordingInput) (int64, error) {
	var id int64
	err := db.execTx(ctx, func(tx *sql.Tx) error {
		// Guard: only one active recording.
		var active int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM recordings WHERE status = ?`, RecordingRecording,
		).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return errors.New("another recording is already active")
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO recordings
			  (event_id, file_path, status, started_at, ffmpeg_pid, ffmpeg_log_path)
			VALUES (?, ?, ?, ?, ?, ?)`,
			nullableText(in.EventID), in.FilePath, RecordingRecording,
			time.Now().UTC().Unix(), in.FFmpegPID, in.FFmpegLogPath,
		)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// StopRecording transitions the active recording to 'stopped' (or 'failed' if
// stopReason indicates an abnormal exit) and returns it so callers can decide
// what to do next (usually: enqueue for upload).
func (db *DB) StopRecording(ctx context.Context, stopReason string) (*Recording, error) {
	var rec *Recording
	err := db.execTx(ctx, func(tx *sql.Tx) error {
		// Find the active row.
		row := tx.QueryRowContext(ctx, `
			SELECT id, event_id, file_path, status, started_at, stopped_at,
			       stop_reason, ffmpeg_pid, ffmpeg_log_path, notes
			FROM recordings
			WHERE status = ?
			ORDER BY started_at DESC
			LIMIT 1`, RecordingRecording)

		r, err := scanRecording(row)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNoActiveRecording
			}
			return err
		}
		now := time.Now().UTC().Unix()
		finalStatus := RecordingStopped
		if stopReason == StopReasonFFmpegDied {
			finalStatus = RecordingFailed
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE recordings
			SET status = ?, stopped_at = ?, stop_reason = ?, updated_at = ?
			WHERE id = ?`,
			finalStatus, now, stopReason, now, r.ID,
		); err != nil {
			return err
		}
		// Refresh local struct for return.
		r.Status = finalStatus
		r.StoppedAt = sql.NullTime{Time: time.Unix(now, 0).UTC(), Valid: true}
		r.StopReason = sql.NullString{String: stopReason, Valid: true}
		rec = r
		return nil
	})
	return rec, err
}

// ActiveRecording returns the current active recording, or ErrNoActiveRecording.
func (db *DB) ActiveRecording(ctx context.Context) (*Recording, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, event_id, file_path, status, started_at, stopped_at,
		       stop_reason, ffmpeg_pid, ffmpeg_log_path, notes
		FROM recordings
		WHERE status = ?
		ORDER BY started_at DESC
		LIMIT 1`, RecordingRecording)
	r, err := scanRecording(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoActiveRecording
		}
		return nil, err
	}
	return r, nil
}

// RecentRecordings returns the newest N rows for display in TUI.
func (db *DB) RecentRecordings(ctx context.Context, limit int) ([]Recording, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, event_id, file_path, status, started_at, stopped_at,
		       stop_reason, ffmpeg_pid, ffmpeg_log_path, notes
		FROM recordings
		ORDER BY started_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Recording
	for rows.Next() {
		r, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanRecording(r rowScanner) (*Recording, error) {
	var rec Recording
	var startedAt int64
	var stoppedAt sql.NullInt64
	err := r.Scan(
		&rec.ID, &rec.EventID, &rec.FilePath, &rec.Status, &startedAt, &stoppedAt,
		&rec.StopReason, &rec.FFmpegPID, &rec.FFmpegLogPath, &rec.Notes,
	)
	if err != nil {
		return nil, err
	}
	rec.StartedAt = time.Unix(startedAt, 0).UTC()
	if stoppedAt.Valid {
		rec.StoppedAt = sql.NullTime{Time: time.Unix(stoppedAt.Int64, 0).UTC(), Valid: true}
	}
	return &rec, nil
}

// AdoptStaleActiveRecordings marks any 'recording' rows as failed with reason
// 'ffmpeg_died'. Called on daemon startup to clean up after a crash.
func (db *DB) AdoptStaleActiveRecordings(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Unix()
	res, err := db.ExecContext(ctx, `
		UPDATE recordings
		SET status = ?, stopped_at = ?, stop_reason = ?, updated_at = ?
		WHERE status = ?`,
		RecordingFailed, now, StopReasonFFmpegDied, now, RecordingRecording,
	)
	if err != nil {
		return 0, fmt.Errorf("adopt stale: %w", err)
	}
	return res.RowsAffected()
}
