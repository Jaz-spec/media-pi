package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

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
		name     string
		stdout   string
		wantPID  int
		wantFile string
		wantErr  bool
	}{
		{
			name:     "happy path",
			stdout:   "recording pid=1234 file=/home/pi/media-pi/recordings/session_20260101_120000.mp4\n",
			wantPID:  1234,
			wantFile: "/home/pi/media-pi/recordings/session_20260101_120000.mp4",
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
			pid, file, err := parseStartOutput(tc.stdout)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if pid != tc.wantPID || file != tc.wantFile {
				t.Fatalf("got pid=%d file=%q; want pid=%d file=%q", pid, file, tc.wantPID, tc.wantFile)
			}
		})
	}
}

func TestRecorderStartInsertsAndStopEnqueues(t *testing.T) {
	db := newTestDB(t)
	file := "/tmp/session_20260101_120000.mp4"
	exec := &fakeExec{outputs: map[string]execsh.RunResult{
		"scripts/record.sh start": {
			Stdout:   "recording pid=777 file=" + file + "\n",
			ExitCode: 0,
		},
		"scripts/record.sh stop": {
			Stdout:   file + "\n",
			ExitCode: 0,
		},
	}}
	cfg := config.Config{FFmpegInputArgs: "-f v4l2 -i /dev/video0", LogDir: "/tmp/logs"}

	r := NewRecorder(db, exec, cfg)
	ctx := context.Background()

	rec, err := r.Start(ctx, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if rec.FilePath != file {
		t.Fatalf("wrong file path: %s", rec.FilePath)
	}
	if rec.FFmpegPID.Int64 != 777 {
		t.Fatalf("wrong pid: %d", rec.FFmpegPID.Int64)
	}

	// Starting again should error because DB has an active row.
	if _, err := r.Start(ctx, ""); err == nil {
		t.Fatalf("expected second Start to error")
	}

	res, err := r.Stop(ctx, "")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if res.Recording.Status != state.RecordingStopped {
		t.Fatalf("expected stopped; got %q", res.Recording.Status)
	}
	if res.UploadID == 0 {
		t.Fatalf("expected upload to be enqueued")
	}
	// Upload row should exist, be pending, and linked to the recording.
	up, err := db.NextPendingUpload(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if up.ID != res.UploadID {
		t.Fatalf("upload id mismatch")
	}
	if !up.RecordingID.Valid || up.RecordingID.Int64 != res.Recording.ID {
		t.Fatalf("upload.recording_id should link to the recording")
	}
}

func TestMirrorLogPath(t *testing.T) {
	got := mirrorLogPath("/home/pi/media-pi/recordings/session_20260101_120000.mp4", "/home/pi/media-pi/logs")
	want := "/home/pi/media-pi/logs/session_20260101_120000.log"
	if got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}
