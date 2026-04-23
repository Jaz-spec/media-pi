package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"), false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	db := newTestDB(t)
	// Running Migrate again should be a no-op.
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestEnqueueAndNextPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id1, err := db.EnqueueUpload(ctx, 0, "/tmp/a.mp4")
	if err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	id2, err := db.EnqueueUpload(ctx, 0, "/tmp/b.mp4")
	if err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if id1 >= id2 {
		t.Fatalf("expected ids to increase: %d >= %d", id1, id2)
	}

	up, err := db.NextPendingUpload(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if up.ID != id1 {
		t.Fatalf("expected oldest first; got id=%d want=%d", up.ID, id1)
	}
	if up.Status != UploadPending {
		t.Fatalf("expected status pending; got %q", up.Status)
	}
}

func TestEnqueueUniqueConstraint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.EnqueueUpload(ctx, 0, "/tmp/dup.mp4"); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	_, err := db.EnqueueUpload(ctx, 0, "/tmp/dup.mp4")
	if !errors.Is(err, ErrUploadExists) {
		t.Fatalf("expected ErrUploadExists; got %v", err)
	}
}

func TestNextPendingEmpty(t *testing.T) {
	db := newTestDB(t)
	_, err := db.NextPendingUpload(context.Background())
	if !errors.Is(err, ErrNoPending) {
		t.Fatalf("expected ErrNoPending; got %v", err)
	}
}

func TestMarkStartedTransitionsStatusAndAudit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, err := db.EnqueueUpload(ctx, 0, "/tmp/x.mp4")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	attempt, err := db.MarkUploadStarted(ctx, id, "/tmp/x.log")
	if err != nil {
		t.Fatalf("mark started: %v", err)
	}
	if attempt != 1 {
		t.Fatalf("first attempt should be 1; got %d", attempt)
	}

	// Should no longer be pending.
	if _, err := db.NextPendingUpload(ctx); !errors.Is(err, ErrNoPending) {
		t.Fatalf("expected no pending after start; got %v", err)
	}

	// Audit row exists.
	var rowCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM upload_attempts WHERE upload_id = ?`, id,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected 1 audit row; got %d", rowCount)
	}

	// Second attempt on a non-pending row should fail (it's in uploading now).
	if _, err := db.MarkUploadStarted(ctx, id, "/tmp/x.log"); err == nil {
		t.Fatalf("expected error starting a non-pending row")
	}
}

func TestMarkFinishedSuccess(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, _ := db.EnqueueUpload(ctx, 0, "/tmp/s.mp4")
	_, _ = db.MarkUploadStarted(ctx, id, "/tmp/s.log")

	if err := db.MarkUploadFinished(ctx, id, 0, ""); err != nil {
		t.Fatalf("mark finished: %v", err)
	}

	ups, err := db.ListUploads(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ups) != 1 {
		t.Fatalf("expected 1 upload; got %d", len(ups))
	}
	if ups[0].Status != UploadUploaded {
		t.Fatalf("expected status uploaded; got %q", ups[0].Status)
	}
	if ups[0].LastError.Valid {
		t.Fatalf("expected null last_error on success; got %q", ups[0].LastError.String)
	}
}

func TestMarkFinishedFailureThenRetry(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, _ := db.EnqueueUpload(ctx, 0, "/tmp/f.mp4")
	_, _ = db.MarkUploadStarted(ctx, id, "/tmp/f.log")
	if err := db.MarkUploadFinished(ctx, id, 3, "PUT exhausted"); err != nil {
		t.Fatalf("mark finished fail: %v", err)
	}
	ups, _ := db.ListUploads(ctx, 10)
	if ups[0].Status != UploadFailed {
		t.Fatalf("expected failed; got %q", ups[0].Status)
	}
	if !ups[0].LastError.Valid || ups[0].LastError.String != "PUT exhausted" {
		t.Fatalf("expected last_error persisted; got %+v", ups[0].LastError)
	}

	// Retry transitions back to pending, preserving attempt_count.
	if err := db.RetryUpload(ctx, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	pending, err := db.NextPendingUpload(ctx)
	if err != nil {
		t.Fatalf("next after retry: %v", err)
	}
	if pending.ID != id {
		t.Fatalf("retry returned wrong row: %d", pending.ID)
	}
	if pending.AttemptCount != 1 {
		t.Fatalf("attempt count should still be 1 before next start; got %d", pending.AttemptCount)
	}

	// Second attempt increments the counter.
	attempt, err := db.MarkUploadStarted(ctx, id, "/tmp/f2.log")
	if err != nil {
		t.Fatalf("second mark started: %v", err)
	}
	if attempt != 2 {
		t.Fatalf("second attempt should be 2; got %d", attempt)
	}
}

func TestRetryOnlyFromFailed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, _ := db.EnqueueUpload(ctx, 0, "/tmp/p.mp4")
	// Pending → retry: should fail (it's not failed, it's pending).
	if err := db.RetryUpload(ctx, id); err == nil {
		t.Fatalf("retry from pending should error")
	}
}
