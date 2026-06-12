package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/orchestrator"
	"github.com/foundersandcoders/media-pi/internal/platform"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

// fakeExec emulates record.sh with adjustable exit codes / outputs.
// startSession is the path returned for start/stop (a session directory in
// the new segment-muxer world).
type fakeExec struct {
	startSession string
}

func (f *fakeExec) RunScript(_ context.Context, script string, args ...string) (execsh.RunResult, error) {
	if len(args) == 0 {
		return execsh.RunResult{}, nil
	}
	switch args[0] {
	case "start":
		return execsh.RunResult{
			Stdout:   "recording pid=1234 session=" + f.startSession + "\n",
			ExitCode: 0,
		}, nil
	case "stop":
		return execsh.RunResult{Stdout: f.startSession + "\n", ExitCode: 0}, nil
	case "status":
		return execsh.RunResult{Stdout: "recording pid=1234 session=" + f.startSession}, nil
	}
	return execsh.RunResult{ExitCode: 1}, nil
}

// seedSession drops one sealed part file for the given session prefix into
// the prefix's parent dir. Used by tests that expect Stop to enqueue an
// upload for the session.
func seedSession(t *testing.T, sessionPrefix string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sessionPrefix), 0o755); err != nil {
		t.Fatalf("mkdir recordings: %v", err)
	}
	if err := os.WriteFile(sessionPrefix+"_part_001.mp4", []byte("x"), 0o644); err != nil {
		t.Fatalf("write part: %v", err)
	}
}

func (f *fakeExec) StreamScript(_ context.Context, _ string, _ string, _ ...string) (int, error) {
	return 0, nil
}

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "s.db"), false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// httpJSON returns an httptest.Server that answers /pi/X/upcoming-events with
// the supplied events.
func mockPlatform(events []platform.Event) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(events)
	}))
}

func TestPollOnceUpsertsEvents(t *testing.T) {
	db := newTestDB(t)
	start := time.Now().Add(1 * time.Hour).UTC().Round(time.Second)
	end := start.Add(1 * time.Hour)
	srv := mockPlatform([]platform.Event{
		{ID: "e1", WorkshopName: "Algebra", StartTime: start, EndTime: end},
	})
	defer srv.Close()

	cfg := config.Config{
		FACAPIBaseURL:        srv.URL,
		FACPiID:              "pi-1",
		FACAPIKey:            "k",
		SchedulePollInterval: 100 * time.Millisecond,
		ScheduleLookahead:    48 * time.Hour,
		ScheduleLookbehind:   1 * time.Hour,
		SchedulerTick:        100 * time.Millisecond,
	}
	s := New(db, cfg, platform.New(cfg), nil, nil)

	if err := s.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	got, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if got.WorkshopName != "Algebra" {
		t.Fatalf("unexpected event: %+v", got)
	}
}

func TestTickFiresDueEvent(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	// Seed an event whose start is now and end is in the future.
	_ = db.UpsertEvent(context.Background(), state.Event{
		ID: "e1", WorkshopName: "X",
		StartTime: now.Add(-time.Second), EndTime: now.Add(time.Hour),
	})

	sessionDir := filepath.Join(t.TempDir(), "session_e1")
	seedSession(t, sessionDir)
	exec := &fakeExec{startSession: sessionDir}
	cfg := config.Config{
		FFmpegInputArgs: "x",
		LogDir:          "/tmp/logs",
		SchedulerTick:   50 * time.Millisecond,
		// No platform config so Run won't actually poll; we call tickOnce directly.
	}
	recorder := orchestrator.NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)

	s := New(db, cfg, platform.New(cfg), recorder, up)
	s.tickOnce(context.Background())

	got, _ := db.GetEvent(context.Background(), "e1")
	if got.TriggerStatus != state.TriggerFired {
		t.Fatalf("expected fired; got %q", got.TriggerStatus)
	}
	if !got.RecordingID.Valid {
		t.Fatalf("expected recording_id linked")
	}
	if active, err := db.ActiveRecording(context.Background()); err != nil || active == nil {
		t.Fatalf("expected active recording after fire: err=%v rec=%v", err, active)
	}
}

func TestTickPreemptsManualRecording(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	tmpRoot := t.TempDir()
	manualPrefix := filepath.Join(tmpRoot, "session_manual")
	schedPrefix := filepath.Join(tmpRoot, "session_sched")
	seedSession(t, manualPrefix)
	seedSession(t, schedPrefix)

	// A manual recording is already active.
	_, err := db.StartRecording(ctx, state.NewRecordingInput{
		FilePath: manualPrefix, FFmpegPID: 111,
	})
	if err != nil {
		t.Fatalf("prep: %v", err)
	}

	now := time.Now().UTC()
	_ = db.UpsertEvent(ctx, state.Event{
		ID: "sched", WorkshopName: "X",
		StartTime: now.Add(-time.Second), EndTime: now.Add(time.Hour),
	})

	exec := &fakeExec{startSession: schedPrefix}
	cfg := config.Config{FFmpegInputArgs: "x", LogDir: "/tmp/logs"}
	recorder := orchestrator.NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)
	s := New(db, cfg, platform.New(cfg), recorder, up)

	s.tickOnce(ctx)

	got, _ := db.GetEvent(ctx, "sched")
	if got.TriggerStatus != state.TriggerFired {
		t.Fatalf("expected fired after pre-empt; got %q", got.TriggerStatus)
	}
	// Active recording should now be the scheduled one.
	active, err := db.ActiveRecording(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if active.FilePath != schedPrefix {
		t.Fatalf("expected scheduled session to be active; got %s", active.FilePath)
	}
}

func TestTickStopsEventAtEndTime(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Event already fired, end_time now past.
	_ = db.UpsertEvent(ctx, state.Event{
		ID: "ending", WorkshopName: "X",
		StartTime: time.Now().Add(-time.Hour), EndTime: time.Now().Add(-time.Second),
	})
	_ = db.UpdateEventTrigger(ctx, "ending", state.TriggerFired, 0)

	schedPrefix := filepath.Join(t.TempDir(), "session_sched")
	seedSession(t, schedPrefix)

	// And a currently-active recording to stop.
	_, _ = db.StartRecording(ctx, state.NewRecordingInput{
		FilePath: schedPrefix, FFmpegPID: 5,
	})

	exec := &fakeExec{startSession: schedPrefix}
	cfg := config.Config{FFmpegInputArgs: "x", LogDir: "/tmp/logs"}
	recorder := orchestrator.NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)
	s := New(db, cfg, platform.New(cfg), recorder, up)

	s.tickOnce(ctx)

	// Recording should now be stopped + segment enqueued.
	if _, err := db.ActiveRecording(ctx); err == nil {
		t.Fatalf("expected no active recording after scheduled_end")
	}
	// Upload row should exist for the seeded part.
	u, err := db.NextPendingUpload(ctx)
	if err != nil {
		t.Fatalf("expected upload enqueued: %v", err)
	}
	wantPart := schedPrefix + "_part_001.mp4"
	if u.FilePath != wantPart {
		t.Fatalf("wrong upload file: %s (want %s)", u.FilePath, wantPart)
	}
}
