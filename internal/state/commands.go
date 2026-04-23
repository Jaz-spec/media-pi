package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Command kinds.
const (
	CmdStartRecording   = "start_recording"
	CmdStopRecording    = "stop_recording"
	CmdRetryUpload      = "retry_upload"
	CmdDismissFailed    = "dismiss_failed"
	CmdResolveInterlock = "resolve_interlock"
)

// Command statuses.
const (
	CmdPending  = "pending"
	CmdAccepted = "accepted"
	CmdDone     = "done"
	CmdError    = "error"
)

// Command is the in-Go view of a commands row.
type Command struct {
	ID          int64
	Kind        string
	Payload     sql.NullString
	Status      string
	Result      sql.NullString
	CreatedAt   time.Time
	ProcessedAt sql.NullTime
}

// InsertCommand queues a new command for the daemon. Returns the id.
// Payload is the caller's responsibility to JSON-encode; empty string means
// "no payload."
func (db *DB) InsertCommand(ctx context.Context, kind, payload string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO commands (kind, payload, status) VALUES (?, ?, ?)`,
		kind, nullableText(payload), CmdPending,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// NextPendingCommand returns the oldest pending command, or ErrNoPending.
// Same error type as NextPendingUpload so callers can handle uniformly.
func (db *DB) NextPendingCommand(ctx context.Context) (*Command, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, kind, payload, status, result, created_at, processed_at
		FROM commands
		WHERE status = ?
		ORDER BY id
		LIMIT 1`, CmdPending)
	return scanCommand(row)
}

// MarkCommandDone records successful completion and optionally a result blob.
func (db *DB) MarkCommandDone(ctx context.Context, id int64, result string) error {
	return db.finaliseCommand(ctx, id, CmdDone, result)
}

// MarkCommandError records failure; the result should contain enough detail
// for the TUI to surface.
func (db *DB) MarkCommandError(ctx context.Context, id int64, result string) error {
	return db.finaliseCommand(ctx, id, CmdError, result)
}

// MarkCommandStatus sets an arbitrary status + result. Used for the
// interlock "accepted but waiting" state, where a start_recording command
// is paused until the TUI sends a resolve_interlock.
func (db *DB) MarkCommandStatus(ctx context.Context, id int64, status, result string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE commands SET status = ?, result = ? WHERE id = ?`,
		status, nullableText(result), id,
	)
	return err
}

func (db *DB) finaliseCommand(ctx context.Context, id int64, status, result string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE commands
		 SET status = ?, result = ?, processed_at = ?
		 WHERE id = ?`,
		status, nullableText(result), time.Now().UTC().Unix(), id,
	)
	return err
}

// GetCommand returns a single command by id (used by TUI to poll for result).
func (db *DB) GetCommand(ctx context.Context, id int64) (*Command, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, kind, payload, status, result, created_at, processed_at
		FROM commands WHERE id = ?`, id)
	return scanCommand(row)
}

func scanCommand(r rowScanner) (*Command, error) {
	var c Command
	var createdAt int64
	var processedAt sql.NullInt64
	err := r.Scan(
		&c.ID, &c.Kind, &c.Payload, &c.Status, &c.Result,
		&createdAt, &processedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoPending
		}
		return nil, err
	}
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	if processedAt.Valid {
		c.ProcessedAt = sql.NullTime{Time: time.Unix(processedAt.Int64, 0).UTC(), Valid: true}
	}
	return &c, nil
}
