package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

func TestCommandConsumerDispatchesStartStop(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "session_20260101_120000")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Drop one sealed part so Stop has something to flush.
	part := filepath.Join(sessionDir, "part_001.mp4")
	if err := os.WriteFile(part, []byte("x"), 0o644); err != nil {
		t.Fatalf("write part: %v", err)
	}

	exec := &fakeExec{outputs: map[string]execsh.RunResult{
		"scripts/record.sh start": {
			Stdout:   fmt.Sprintf("recording pid=1001 session=%s\n", sessionDir),
			ExitCode: 0,
		},
		"scripts/record.sh stop": {
			Stdout:   sessionDir + "\n",
			ExitCode: 0,
		},
	}}
	cfg := config.Config{FFmpegInputArgs: "-f v4l2 -i /dev/video0", LogDir: "/tmp/logs"}
	recorder := NewRecorder(db, exec, cfg)
	recorder.segmentPollInterval = 20 * time.Millisecond
	up := worker.NewUpload(db, exec, cfg)

	consumer := NewCommandConsumer(db, recorder, up, cfg)
	consumer.tick = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = consumer.Run(ctx)
		close(done)
	}()

	// Insert a start command; wait for it to finish.
	startID, err := db.InsertCommand(ctx, state.CmdStartRecording, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, startID)
		return c != nil && c.Status != state.CmdPending && c.Status != state.CmdAccepted
	})
	c, _ := db.GetCommand(ctx, startID)
	if c.Status != state.CmdDone {
		t.Fatalf("start not done: status=%s result=%v", c.Status, c.Result)
	}
	if _, err := db.ActiveRecording(ctx); err != nil {
		t.Fatalf("expected active recording after start: %v", err)
	}

	// Now stop.
	stopID, _ := db.InsertCommand(ctx, state.CmdStopRecording, `{}`)
	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, stopID)
		return c != nil && c.Status == state.CmdDone
	})
	// The lone sealed part should be enqueued.
	u, err := db.NextPendingUpload(ctx)
	if err != nil {
		t.Fatalf("expected upload enqueued: %v", err)
	}
	if u.FilePath != part {
		t.Fatalf("wrong file: %s (want %s)", u.FilePath, part)
	}

	cancel()
	<-done
}

func TestCommandConsumerHandlesRetry(t *testing.T) {
	db := newTestDB(t)
	// Put a failed upload in place.
	ctx := context.Background()
	id, _ := db.EnqueueUpload(ctx, 0, "/tmp/f.mp4")
	_, _ = db.MarkUploadStarted(ctx, id, "/tmp/f.log")
	_ = db.MarkUploadFinished(ctx, id, 3, "boom")

	exec := &fakeExec{}
	cfg := config.Config{FFmpegInputArgs: "x", LogDir: "/tmp"}
	recorder := NewRecorder(db, exec, cfg)
	up := worker.NewUpload(db, exec, cfg)

	consumer := NewCommandConsumer(db, recorder, up, cfg)
	consumer.tick = 10 * time.Millisecond

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = consumer.Run(cctx)
		close(done)
	}()

	cmdID, _ := db.InsertCommand(ctx, state.CmdRetryUpload,
		fmt.Sprintf(`{"upload_id":%d}`, id))

	waitFor(t, 2*time.Second, func() bool {
		c, _ := db.GetCommand(ctx, cmdID)
		return c != nil && c.Status == state.CmdDone
	})

	ups, _ := db.ListUploads(ctx, 10)
	if ups[0].Status != state.UploadPending {
		t.Fatalf("expected pending after retry; got %q", ups[0].Status)
	}

	cancel()
	<-done
}

func waitFor(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
