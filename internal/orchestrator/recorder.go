// Package orchestrator wraps the execution primitives (bash scripts, worker,
// scheduler) behind a single API that the daemon and CLI use.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
)

// Recorder wraps record.sh. It inserts / updates recordings rows and, on stop,
// enqueues the finalised file for upload.
type Recorder struct {
	db     *state.DB
	exec   execsh.Execer
	cfg    config.Config
	script string // relative path to record.sh
}

// NewRecorder constructs a Recorder. The caller is expected to hand in an
// Execer rooted at the repo root.
func NewRecorder(db *state.DB, exec execsh.Execer, cfg config.Config) *Recorder {
	return &Recorder{
		db:     db,
		exec:   exec,
		cfg:    cfg,
		script: "scripts/record.sh",
	}
}

// Start invokes `record.sh start`, parses the resulting stdout to learn the
// ffmpeg pid and output file, and inserts a recordings row. EventID is
// optional; empty = manual recording.
func (r *Recorder) Start(ctx context.Context, eventID string) (*state.Recording, error) {
	if err := r.cfg.RequireRecordConfig(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// Fail early if DB says we're already recording.
	if _, err := r.db.ActiveRecording(ctx); err == nil {
		return nil, errors.New("another recording is already active")
	} else if !errors.Is(err, state.ErrNoActiveRecording) {
		return nil, err
	}

	res, err := r.exec.RunScript(ctx, r.script, "start")
	if err != nil {
		return nil, fmt.Errorf("exec record.sh start: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("record.sh start exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	pid, file, err := parseStartOutput(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse record.sh output: %w (stdout=%q)", err, res.Stdout)
	}

	// Log path follows record.sh's convention: same basename as the mp4, .log
	// in the LOG_DIR. Mirrored here so we can present it in the TUI later.
	logPath := mirrorLogPath(file, r.cfg.LogDir)

	id, err := r.db.StartRecording(ctx, state.NewRecordingInput{
		EventID:       eventID,
		FilePath:      file,
		FFmpegPID:     int64(pid),
		FFmpegLogPath: logPath,
	})
	if err != nil {
		return nil, fmt.Errorf("record start insert: %w", err)
	}
	rec, err := r.db.ActiveRecording(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch new recording id=%d: %w", id, err)
	}
	return rec, nil
}

// StopResult is what the caller gets back from Stop: the updated recording
// row plus the id of the upload row that was enqueued for this file.
type StopResult struct {
	Recording *state.Recording
	UploadID  int64
}

// Stop invokes `record.sh stop` (sends SIGINT to ffmpeg, waits for clean
// finalisation), updates the DB, and enqueues the finalised mp4 for upload.
func (r *Recorder) Stop(ctx context.Context, reason string) (*StopResult, error) {
	if reason == "" {
		reason = state.StopReasonManual
	}
	res, err := r.exec.RunScript(ctx, r.script, "stop")
	if err != nil {
		return nil, fmt.Errorf("exec record.sh stop: %w", err)
	}
	if res.ExitCode != 0 {
		// record.sh stop exits 1 if it thought nothing was recording — but
		// still prints the last filename on stdout. Surface the warning but
		// don't abort: if the DB thought we were recording, there's a row to
		// close.
		if strings.TrimSpace(res.Stdout) == "" {
			return nil, fmt.Errorf("record.sh stop exited %d, no file: %s",
				res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
	file := strings.TrimSpace(res.Stdout)
	if file == "" {
		return nil, errors.New("record.sh stop returned no filename")
	}

	rec, err := r.db.StopRecording(ctx, reason)
	if err != nil {
		// If the DB has no active row but the script knew a filename, we
		// still want to enqueue the upload — the file is on disk.
		if !errors.Is(err, state.ErrNoActiveRecording) {
			return nil, fmt.Errorf("record stop update: %w", err)
		}
	}

	uploadID, err := r.db.EnqueueUpload(ctx, recordingIDOrZero(rec), file)
	if err != nil {
		if errors.Is(err, state.ErrUploadExists) {
			// Already queued — benign on re-stop.
			return &StopResult{Recording: rec}, nil
		}
		return nil, fmt.Errorf("enqueue upload: %w", err)
	}
	return &StopResult{Recording: rec, UploadID: uploadID}, nil
}

// Status calls `record.sh status` and returns a human string. Useful for CLI
// and for the TUI's status pane.
func (r *Recorder) Status(ctx context.Context) (string, error) {
	res, err := r.exec.RunScript(ctx, r.script, "status")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// startOutputRe matches the `recording pid=1234 file=/path/to/foo.mp4` line
// record.sh prints on successful start.
var startOutputRe = regexp.MustCompile(`recording\s+pid=(\d+)\s+file=(\S+)`)

func parseStartOutput(stdout string) (pid int, file string, err error) {
	m := startOutputRe.FindStringSubmatch(stdout)
	if m == nil {
		return 0, "", errors.New("did not find `recording pid=... file=...` line")
	}
	pid, err = strconv.Atoi(m[1])
	if err != nil {
		return 0, "", fmt.Errorf("pid parse: %w", err)
	}
	return pid, m[2], nil
}

// mirrorLogPath reproduces record.sh's convention: replace the mp4 basename's
// `.mp4` extension with `.log` and move it into the LOG_DIR. Used for display.
func mirrorLogPath(mp4Path, logDir string) string {
	base := mp4Path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[:i]
	}
	return fmt.Sprintf("%s/%s.log", strings.TrimSuffix(logDir, "/"), base)
}

func recordingIDOrZero(r *state.Recording) int64 {
	if r == nil {
		return 0
	}
	return r.ID
}
