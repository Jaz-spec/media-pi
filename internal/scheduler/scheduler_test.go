package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
type fakeExec struct {
	startFile string
}

func (f *fakeExec) RunScript(_ context.Context, script string, args ...string) (execsh.RunResult, error) {
	if len(args) == 0 {
		return execsh.RunResult{}, nil
	}
	switch args[0] {
	case "start":
		return execsh.RunResult{
			Stdout:   "recording pid=1234 file=" + f.startFile + "\n",
			ExitCode: 0,
		}, nil
	case "stop":
		return execsh.RunResult{Stdout: f.startFile + "\n", ExitCode: 0}, nil
	case "status":
		return execsh.RunResult{Stdout: "recording pid=1234 file=" + f.startFile}, nil
	}
	return execsh.RunResult{ExitCode: 1}, nil
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

	exec := &fakeExec{startFile: "/tmp/e1.mp4"}
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

	// A manual recording is already active.
	_, err := db.StartRecording(ctx, state.NewRecordingInput{
		FilePath: "/tmp/manual.mp4", FFmpegPID: 111,
	})
	if err != nil {
		t.Fatalf("prep: %v", err)
	}

	now := time.Now().UTC()
	_ = db.UpsertEvent(ctx, state.Event{
		ID: "sched", WorkshopName: "X",
		StartTime: now.Add(-time.Second), EndTime: now.Add(time.Hour),
	})

	exec := &fakeExec{startFile: "/tmp/sched.mp4"}
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
	if active.FilePath != "/tmp/sched.mp4" {
		t.Fatalf("expected scheduled file to be active; got %s", active.FilePath)
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

	// And a currently-active recording to stop.
	_, _ = db.StartRecording(ctx, state.NewRecordingInput{
		FilePath: "/tmp/sched.mp4", FFmpegPID: 5,
	})

	exec := &fakeExec{startFile: "/tmp/sched.mp4"}
	cfg := config.Config{FFmpegInputArgs: "x", LogDir: "/tmp/logs"}
	recorder := orchestrator.NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)
	s := New(db, cfg, platform.New(cfg), recorder, up)

	s.tickOnce(ctx)

	// Recording should now be stopped + upload enqueued.
	if _, err := db.ActiveRecording(ctx); err == nil {
		t.Fatalf("expected no active recording after scheduled_end")
	}
	// Upload row should exist.
	u, err := db.NextPendingUpload(ctx)
	if err != nil {
		t.Fatalf("expected upload enqueued: %v", err)
	}
	if u.FilePath != "/tmp/sched.mp4" {
		t.Fatalf("wrong upload file: %s", u.FilePath)
	}
}
