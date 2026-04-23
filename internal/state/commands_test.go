package state

import (
	"context"
	"errors"
	"testing"
)

func TestCommandLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Empty queue.
	if _, err := db.NextPendingCommand(ctx); !errors.Is(err, ErrNoPending) {
		t.Fatalf("expected ErrNoPending; got %v", err)
	}

	id, err := db.InsertCommand(ctx, CmdStartRecording, `{"event_id":"evt-1"}`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	cmd, err := db.NextPendingCommand(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if cmd.ID != id || cmd.Kind != CmdStartRecording {
		t.Fatalf("unexpected cmd: %+v", cmd)
	}
	if !cmd.Payload.Valid || cmd.Payload.String != `{"event_id":"evt-1"}` {
		t.Fatalf("payload round-trip broken: %+v", cmd.Payload)
	}

	// Mark done; should disappear from pending.
	if err := db.MarkCommandDone(ctx, id, `{"recording_id":42}`); err != nil {
		t.Fatalf("done: %v", err)
	}
	if _, err := db.NextPendingCommand(ctx); !errors.Is(err, ErrNoPending) {
		t.Fatalf("expected ErrNoPending after done; got %v", err)
	}
	got, err := db.GetCommand(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != CmdDone || !got.Result.Valid {
		t.Fatalf("unexpected final cmd: %+v", got)
	}
}

func TestCommandError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	id, _ := db.InsertCommand(ctx, CmdRetryUpload, `{"upload_id":1}`)
	if err := db.MarkCommandError(ctx, id, `{"error":"boom"}`); err != nil {
		t.Fatalf("error: %v", err)
	}
	got, _ := db.GetCommand(ctx, id)
	if got.Status != CmdError {
		t.Fatalf("expected error status; got %q", got.Status)
	}
}

func TestCommandFIFOOrder(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	id1, _ := db.InsertCommand(ctx, CmdStartRecording, "")
	id2, _ := db.InsertCommand(ctx, CmdStopRecording, "")

	c1, _ := db.NextPendingCommand(ctx)
	if c1.ID != id1 {
		t.Fatalf("expected first inserted first; got %d", c1.ID)
	}
	_ = db.MarkCommandDone(ctx, c1.ID, "")
	c2, _ := db.NextPendingCommand(ctx)
	if c2.ID != id2 {
		t.Fatalf("expected second next; got %d", c2.ID)
	}
}
