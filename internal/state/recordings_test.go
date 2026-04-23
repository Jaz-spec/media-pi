package state

import (
	"context"
	"errors"
	"testing"
)

func TestStartRecordingInsertsRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, err := db.StartRecording(ctx, NewRecordingInput{
		FilePath:      "/tmp/session_1.mp4",
		FFmpegPID:     1234,
		FFmpegLogPath: "/tmp/session_1.log",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id; got %d", id)
	}
	active, err := db.ActiveRecording(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if active.ID != id || active.Status != RecordingRecording {
		t.Fatalf("unexpected active row: %+v", active)
	}
}

func TestStartRejectsSecondActive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.StartRecording(ctx, NewRecordingInput{FilePath: "/tmp/a.mp4", FFmpegPID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartRecording(ctx, NewRecordingInput{FilePath: "/tmp/b.mp4", FFmpegPID: 2}); err == nil {
		t.Fatalf("expected second StartRecording to fail")
	}
}

func TestStopRecordingTransitionsStatus(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = db.StartRecording(ctx, NewRecordingInput{FilePath: "/tmp/s.mp4", FFmpegPID: 5})

	rec, err := db.StopRecording(ctx, StopReasonManual)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if rec.Status != RecordingStopped {
		t.Fatalf("expected stopped; got %q", rec.Status)
	}
	if !rec.StoppedAt.Valid {
		t.Fatalf("stopped_at should be set")
	}
	// No longer active.
	if _, err := db.ActiveRecording(ctx); !errors.Is(err, ErrNoActiveRecording) {
		t.Fatalf("expected ErrNoActiveRecording; got %v", err)
	}
}

func TestStopWithFFmpegDiedMarksFailed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = db.StartRecording(ctx, NewRecordingInput{FilePath: "/tmp/d.mp4", FFmpegPID: 9})
	rec, err := db.StopRecording(ctx, StopReasonFFmpegDied)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if rec.Status != RecordingFailed {
		t.Fatalf("expected failed on ffmpeg_died; got %q", rec.Status)
	}
}

func TestAdoptStaleClearsActiveOnStartup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, _ = db.StartRecording(ctx, NewRecordingInput{FilePath: "/tmp/x.mp4", FFmpegPID: 11})
	n, err := db.AdoptStaleActiveRecordings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 adopted; got %d", n)
	}
	if _, err := db.ActiveRecording(ctx); !errors.Is(err, ErrNoActiveRecording) {
		t.Fatalf("expected no active after adopt; got %v", err)
	}
}
