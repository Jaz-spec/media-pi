package worker

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
)

// fakeExec implements execsh.Execer. Each Upload row's file path maps to a
// fixed exit code; unknown paths default to 0.
type fakeExec struct {
	mu        sync.Mutex
	exitBy    map[string]int
	callLog   []string
}

func (f *fakeExec) RunScript(_ context.Context, script string, args ...string) (execsh.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callLog = append(f.callLog, script)
	return execsh.RunResult{ExitCode: 0}, nil
}

func (f *fakeExec) StreamScript(_ context.Context, logPath, script string, args ...string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callLog = append(f.callLog, script+" "+args[0])
	if f.exitBy == nil {
		return 0, nil
	}
	return f.exitBy[args[0]], nil
}

func newFakeExec(exits map[string]int) *fakeExec {
	return &fakeExec{exitBy: exits}
}

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "t.db"), false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func runWorker(t *testing.T, w *Upload, until func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for {
		if until() {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("timed out waiting for worker condition")
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWorkerProcessesSingleSuccess(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Config{LogDir: t.TempDir()}
	exec := newFakeExec(map[string]int{"/tmp/ok.mp4": 0})

	id, _ := db.EnqueueUpload(context.Background(), 0, "/tmp/ok.mp4")

	w := NewUpload(db, exec, cfg)
	// Shorten idle so the test doesn't need a Wake() to drive progress.
	w.idleTick = 10 * time.Millisecond

	runWorker(t, w, func() bool {
		ups, err := db.ListUploads(context.Background(), 10)
		if err != nil {
			return false
		}
		return len(ups) == 1 && ups[0].Status == state.UploadUploaded
	})

	// Final state assertions.
	ups, _ := db.ListUploads(context.Background(), 10)
	if ups[0].ID != id {
		t.Fatalf("wrong row processed")
	}
	if ups[0].AttemptCount != 1 {
		t.Fatalf("expected 1 attempt; got %d", ups[0].AttemptCount)
	}
}

func TestWorkerProcessesSerially(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Config{LogDir: t.TempDir()}
	exec := newFakeExec(nil) // all succeed

	// Enqueue three files.
	for _, p := range []string{"/tmp/a.mp4", "/tmp/b.mp4", "/tmp/c.mp4"} {
		if _, err := db.EnqueueUpload(context.Background(), 0, p); err != nil {
			t.Fatalf("enqueue %s: %v", p, err)
		}
	}
	w := NewUpload(db, exec, cfg)
	w.idleTick = 10 * time.Millisecond

	runWorker(t, w, func() bool {
		ups, _ := db.ListUploads(context.Background(), 10)
		if len(ups) != 3 {
			return false
		}
		for _, u := range ups {
			if u.Status != state.UploadUploaded {
				return false
			}
		}
		return true
	})

	// Verify serial order by checking exec call sequence.
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.callLog) != 3 {
		t.Fatalf("expected 3 calls; got %d: %v", len(exec.callLog), exec.callLog)
	}
	// Enqueue order is FIFO.
	if !endsWith(exec.callLog[0], "/tmp/a.mp4") ||
		!endsWith(exec.callLog[1], "/tmp/b.mp4") ||
		!endsWith(exec.callLog[2], "/tmp/c.mp4") {
		t.Fatalf("expected FIFO order; got %v", exec.callLog)
	}
}

func TestWorkerFailureRecordsError(t *testing.T) {
	db := newTestDB(t)
	cfg := config.Config{LogDir: t.TempDir()}
	// Exit code 3 = PUT exhausted, retryable
	exec := newFakeExec(map[string]int{"/tmp/fail.mp4": 3})

	_, _ = db.EnqueueUpload(context.Background(), 0, "/tmp/fail.mp4")
	w := NewUpload(db, exec, cfg)
	w.idleTick = 10 * time.Millisecond

	runWorker(t, w, func() bool {
		ups, _ := db.ListUploads(context.Background(), 10)
		return len(ups) == 1 && ups[0].Status == state.UploadFailed
	})

	ups, _ := db.ListUploads(context.Background(), 10)
	if !ups[0].LastError.Valid {
		t.Fatalf("expected last_error to be set")
	}
	if ups[0].AttemptCount != 1 {
		t.Fatalf("expected attempt_count=1; got %d", ups[0].AttemptCount)
	}
}

func endsWith(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}
