package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

func TestInterlockFlowYesProceedsSkippingEvent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Upcoming event 15 min away.
	ev := state.Event{
		ID: "evt-1", WorkshopName: "Algebra",
		StartTime: time.Now().Add(15 * time.Minute),
		EndTime:   time.Now().Add(75 * time.Minute),
	}
	if err := db.UpsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	exec := &fakeExec{outputs: map[string]execsh.RunResult{
		"scripts/record.sh start": {
			Stdout:   "recording pid=1001 file=/tmp/manual.mp4\n",
			ExitCode: 0,
		},
	}}
	cfg := config.Config{
		FFmpegInputArgs: "x", LogDir: "/tmp",
		InterlockWindow: 30 * time.Minute,
	}
	recorder := NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)
	consumer := NewCommandConsumer(db, recorder, up, cfg)
	consumer.tick = 10 * time.Millisecond

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = consumer.Run(cctx); close(done) }()

	// 1. Insert start_recording — expect it to transition to 'accepted' with
	//    an interlock_required result.
	startID, _ := db.InsertCommand(ctx, state.CmdStartRecording, "")
	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, startID)
		return c != nil && c.Status == state.CmdAccepted
	})
	cmd, _ := db.GetCommand(ctx, startID)
	var result struct {
		Kind      string `json:"kind"`
		EventID   string `json:"event_id"`
		EventName string `json:"event_name"`
	}
	_ = json.Unmarshal([]byte(cmd.Result.String), &result)
	if result.Kind != "interlock_required" || result.EventID != "evt-1" {
		t.Fatalf("unexpected interlock result: %s", cmd.Result.String)
	}
	// Nothing should be recording yet.
	if _, err := db.ActiveRecording(ctx); err == nil {
		t.Fatalf("recording started despite interlock")
	}

	// 2. Resolve with "yes" — expect event marked skipped_manual + recording started.
	resolveID, _ := db.InsertCommand(ctx, state.CmdResolveInterlock,
		fmt.Sprintf(`{"original_cmd_id":%d,"answer":"yes"}`, startID))
	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, resolveID)
		return c != nil && c.Status == state.CmdDone
	})

	// Original start should now also be done.
	orig, _ := db.GetCommand(ctx, startID)
	if orig.Status != state.CmdDone {
		t.Fatalf("original cmd should be done; got %s", orig.Status)
	}
	// Event marked skipped_manual.
	got, _ := db.GetEvent(ctx, "evt-1")
	if got.TriggerStatus != state.TriggerSkippedManual {
		t.Fatalf("expected skipped_manual; got %q", got.TriggerStatus)
	}
	// Recording active and linked to the event.
	active, err := db.ActiveRecording(ctx)
	if err != nil {
		t.Fatalf("expected active recording: %v", err)
	}
	if !active.EventID.Valid || active.EventID.String != "evt-1" {
		t.Fatalf("recording should link to event; got %+v", active.EventID)
	}

	cancel()
	<-done
}

func TestInterlockFlowNoStartsUnlinked(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ev := state.Event{
		ID: "evt-2", WorkshopName: "Calc",
		StartTime: time.Now().Add(20 * time.Minute),
		EndTime:   time.Now().Add(80 * time.Minute),
	}
	_ = db.UpsertEvent(ctx, ev)

	exec := &fakeExec{outputs: map[string]execsh.RunResult{
		"scripts/record.sh start": {
			Stdout:   "recording pid=2001 file=/tmp/manual2.mp4\n",
			ExitCode: 0,
		},
	}}
	cfg := config.Config{
		FFmpegInputArgs: "x", LogDir: "/tmp",
		InterlockWindow: 30 * time.Minute,
	}
	recorder := NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)
	consumer := NewCommandConsumer(db, recorder, up, cfg)
	consumer.tick = 10 * time.Millisecond

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = consumer.Run(cctx); close(done) }()

	startID, _ := db.InsertCommand(ctx, state.CmdStartRecording, "")
	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, startID)
		return c != nil && c.Status == state.CmdAccepted
	})

	// Resolve with "no".
	resolveID, _ := db.InsertCommand(ctx, state.CmdResolveInterlock,
		fmt.Sprintf(`{"original_cmd_id":%d,"answer":"no"}`, startID))
	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, resolveID)
		return c != nil && c.Status == state.CmdDone
	})

	// Event stays pending — scheduler will pre-empt at start_time.
	got, _ := db.GetEvent(ctx, "evt-2")
	if got.TriggerStatus != state.TriggerPending {
		t.Fatalf("event should stay pending on 'no'; got %q", got.TriggerStatus)
	}
	// Recording active but NOT linked to the event.
	active, err := db.ActiveRecording(ctx)
	if err != nil {
		t.Fatalf("expected active: %v", err)
	}
	if active.EventID.Valid {
		t.Fatalf("recording should not be linked to event on 'no'")
	}

	cancel()
	<-done
}
