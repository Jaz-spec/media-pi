package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
)

// fakeExec implements execsh.Execer with canned script responses keyed by
// "script args[0]" — e.g. `scripts/record.sh start`.
type fakeExec struct {
	outputs map[string]execsh.RunResult
	calls   []string
}

func (f *fakeExec) RunScript(_ context.Context, script string, args ...string) (execsh.RunResult, error) {
	key := script
	if len(args) > 0 {
		key = script + " " + args[0]
	}
	f.calls = append(f.calls, key)
	if r, ok := f.outputs[key]; ok {
		return r, nil
	}
	return execsh.RunResult{ExitCode: 1, Stderr: "not mocked: " + key}, nil
}

func (f *fakeExec) StreamScript(_ context.Context, _ string, _ string, _ ...string) (int, error) {
	return 0, nil
}

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "r.db"), false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestParseStartOutput(t *testing.T) {
	cases := []struct {
		name        string
		stdout      string
		wantPID     int
		wantSession string
		wantErr     bool
	}{
		{
			name:        "happy path",
			stdout:      "recording pid=1234 session=/home/pi/media-pi/recordings/session_20260101_120000\n",
			wantPID:     1234,
			wantSession: "/home/pi/media-pi/recordings/session_20260101_120000",
		},
		{
			name:    "empty",
			stdout:  "",
			wantErr: true,
		},
		{
			name:    "unrecognised format",
			stdout:  "something else\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pid, session, err := parseStartOutput(tc.stdout)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if pid != tc.wantPID || session != tc.wantSession {
				t.Fatalf("got pid=%d session=%q; want pid=%d session=%q", pid, session, tc.wantPID, tc.wantSession)
			}
		})
	}
}

func TestRecorderStartInsertsAndStopEnqueuesAllSegments(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "session_20260101_120000")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	exec := &fakeExec{outputs: map[string]execsh.RunResult{
		"scripts/record.sh start": {
			Stdout:   "recording pid=777 session=" + sessionDir + "\n",
			ExitCode: 0,
		},
		"scripts/record.sh stop": {
			Stdout:   sessionDir + "\n",
			ExitCode: 0,
		},
	}}
	cfg := config.Config{FFmpegInputArgs: "-f v4l2 -i /dev/video0", LogDir: "/tmp/logs"}

	r := NewRecorder(db, exec, cfg)
	// Faster ticks so the mid-recording assertion doesn't hang.
	r.segmentPollInterval = 20 * time.Millisecond
	ctx := context.Background()

	rec, err := r.Start(ctx, "", 0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if rec.FilePath != sessionDir {
		t.Fatalf("wrong session dir: %s", rec.FilePath)
	}
	if rec.FFmpegPID.Int64 != 777 {
		t.Fatalf("wrong pid: %d", rec.FFmpegPID.Int64)
	}

	// Starting again should error because DB has an active row.
	if _, err := r.Start(ctx, "", 0); err == nil {
		t.Fatalf("expected second Start to error")
	}

	// Drop two parts; the watcher should enqueue part_001 once part_002 exists,
	// and leave part_002 (still in-flight) alone until Stop flushes it.
	writeFile(t, filepath.Join(sessionDir, "part_001.mp4"), "x")
	writeFile(t, filepath.Join(sessionDir, "part_002.mp4"), "y")
	waitFor(t, time.Second, func() bool {
		ups, _ := db.ListUploads(ctx, 10)
		return len(ups) == 1
	})

	res, err := r.Stop(ctx, "")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if res.Recording.Status != state.RecordingStopped {
		t.Fatalf("expected stopped; got %q", res.Recording.Status)
	}
	if len(res.UploadIDs) != 1 {
		t.Fatalf("expected stop to flush 1 trailing chunk; got %v", res.UploadIDs)
	}
	if res.UploadID != res.UploadIDs[0] {
		t.Fatalf("UploadID should mirror the last UploadIDs entry")
	}

	ups, err := db.ListUploads(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ups) != 2 {
		t.Fatalf("expected 2 uploads (one per part); got %d", len(ups))
	}
	for _, u := range ups {
		if !u.RecordingID.Valid || u.RecordingID.Int64 != rec.ID {
			t.Fatalf("upload %d not linked to recording", u.ID)
		}
		if u.Status != state.UploadPending {
			t.Fatalf("upload %d expected pending; got %s", u.ID, u.Status)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMirrorLogPath(t *testing.T) {
	got := mirrorLogPath("/home/pi/media-pi/recordings/session_20260101_120000", "/home/pi/media-pi/logs")
	want := "/home/pi/media-pi/logs/session_20260101_120000.log"
	if got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}
