package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Upload status values. Not an enum type for cheap scanning; callers should
// use these constants rather than hard-coded strings.
const (
	UploadPending   = "pending"
	UploadUploading = "uploading"
	UploadUploaded  = "uploaded"
	UploadFailed    = "failed"
)

// Upload is the in-Go view of an uploads row. Nullable DB columns use
// sql.Null* so callers can distinguish "not set yet" from "empty string".
type Upload struct {
	ID            int64
	RecordingID   sql.NullInt64
	FilePath      string
	Status        string
	AttemptCount  int
	LastError     sql.NullString
	LastLogPath   sql.NullString
	EnqueuedAt    time.Time
	StartedAt     sql.NullTime
	FinishedAt    sql.NullTime
}

// ErrUploadExists is returned by Enqueue when the file_path is already queued
// (uploads.file_path has a UNIQUE constraint).
var ErrUploadExists = errors.New("upload already enqueued for this path")

// ErrNoPending is returned by NextPendingUpload when the queue is empty.
var ErrNoPending = errors.New("no pending uploads")

// Enqueue inserts a new pending uploads row. recordingID may be 0 for ad-hoc
// files enqueued via `facpi enqueue`.
func (db *DB) EnqueueUpload(ctx context.Context, recordingID int64, filePath string) (int64, error) {
	var nullableRec sql.NullInt64
	if recordingID > 0 {
		nullableRec = sql.NullInt64{Int64: recordingID, Valid: true}
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO uploads (recording_id, file_path, status)
		 VALUES (?, ?, ?)`,
		nullableRec, filePath, UploadPending,
	)
	if err != nil {
		// modernc.org/sqlite surfaces UNIQUE violations as an error containing
		// "UNIQUE constraint failed". We check substring rather than typed
		// error to avoid coupling to the driver's error type.
		if isUniqueViolation(err) {
			return 0, ErrUploadExists
		}
		return 0, fmt.Errorf("insert upload: %w", err)
	}
	return res.LastInsertId()
}

// NextPendingUpload returns the oldest pending upload, or ErrNoPending.
func (db *DB) NextPendingUpload(ctx context.Context) (*Upload, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, recording_id, file_path, status, attempt_count,
		       last_error, last_log_path, enqueued_at, started_at, finished_at
		FROM uploads
		WHERE status = ?
		ORDER BY enqueued_at, id
		LIMIT 1`, UploadPending)
	return scanUpload(row)
}

// MarkUploadStarted transitions pending → uploading and records the log path
// for this attempt. Returns the new attempt number.
func (db *DB) MarkUploadStarted(ctx context.Context, id int64, logPath string) (attemptNo int, err error) {
	now := time.Now().UTC().Unix()

	err = db.execTx(ctx, func(tx *sql.Tx) error {
		// Bump attempt_count and flip status.
		res, err := tx.ExecContext(ctx, `
			UPDATE uploads
			SET status = ?, attempt_count = attempt_count + 1,
			    started_at = ?, last_log_path = ?, last_error = NULL
			WHERE id = ? AND status = ?`,
			UploadUploading, now, logPath, id, UploadPending,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("upload %d not in pending state", id)
		}
		// Read the new attempt number and insert the audit row.
		if err := tx.QueryRowContext(ctx,
			`SELECT attempt_count FROM uploads WHERE id = ?`, id,
		).Scan(&attemptNo); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO upload_attempts
			  (upload_id, attempt_no, started_at, log_path)
			VALUES (?, ?, ?, ?)`,
			id, attemptNo, now, logPath,
		)
		return err
	})
	return attemptNo, err
}

// MarkUploadFinished records the terminal state of an attempt and transitions
// the upload row to uploaded or failed. exitCode of 0 means success.
func (db *DB) MarkUploadFinished(ctx context.Context, id int64, exitCode int, errMsg string) error {
	now := time.Now().UTC().Unix()
	finalStatus := UploadUploaded
	if exitCode != 0 {
		finalStatus = UploadFailed
	}

	return db.execTx(ctx, func(tx *sql.Tx) error {
		// Close the audit row for the current attempt.
		if _, err := tx.ExecContext(ctx, `
			UPDATE upload_attempts
			SET finished_at = ?, exit_code = ?, error = ?
			WHERE upload_id = ?
			  AND id = (SELECT MAX(id) FROM upload_attempts WHERE upload_id = ?)`,
			now, exitCode, nullableText(errMsg), id, id,
		); err != nil {
			return err
		}

		// Close the uploads row.
		var nullableErr any
		if errMsg != "" {
			nullableErr = errMsg
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE uploads
			SET status = ?, finished_at = ?, last_error = ?
			WHERE id = ?`,
			finalStatus, now, nullableErr, id,
		)
		return err
	})
}

// RetryUpload flips a failed row back to pending. Attempt count is preserved;
// the next attempt increments it.
func (db *DB) RetryUpload(ctx context.Context, id int64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE uploads
		SET status = ?, last_error = NULL, finished_at = NULL
		WHERE id = ? AND status = ?`,
		UploadPending, id, UploadFailed,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("upload %d not in failed state", id)
	}
	return nil
}

// ListUploads returns recent uploads, newest first, capped at limit.
// Used by the TUI dashboard.
func (db *DB) ListUploads(ctx context.Context, limit int) ([]Upload, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, recording_id, file_path, status, attempt_count,
		       last_error, last_log_path, enqueued_at, started_at, finished_at
		FROM uploads
		ORDER BY enqueued_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Upload{}
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// rowScanner matches both *sql.Row and *sql.Rows for scanUpload.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUpload(r rowScanner) (*Upload, error) {
	var u Upload
	var enqueuedAt int64
	var startedAt, finishedAt sql.NullInt64
	err := r.Scan(
		&u.ID, &u.RecordingID, &u.FilePath, &u.Status, &u.AttemptCount,
		&u.LastError, &u.LastLogPath, &enqueuedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoPending
		}
		return nil, err
	}
	u.EnqueuedAt = time.Unix(enqueuedAt, 0).UTC()
	if startedAt.Valid {
		u.StartedAt = sql.NullTime{Time: time.Unix(startedAt.Int64, 0).UTC(), Valid: true}
	}
	if finishedAt.Valid {
		u.FinishedAt = sql.NullTime{Time: time.Unix(finishedAt.Int64, 0).UTC(), Valid: true}
	}
	return &u, nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite doesn't expose typed errors for constraint failures
	// in the same way cgo-sqlite3 does. Substring match is the pragmatic option.
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
