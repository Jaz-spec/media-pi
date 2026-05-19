// Package orchestrator wraps the execution primitives (bash scripts, worker,
// scheduler) behind a single API that the daemon and CLI use.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

// Recorder wraps record.sh. It inserts / updates recordings rows and, while a
// recording is active, watches the session directory for sealed segments to
// enqueue for upload.
type Recorder struct {
	db     *state.DB
	exec   execsh.Execer
	cfg    config.Config
	script string // relative path to record.sh

	// segmentPollInterval is how often the watcher scans the session dir.
	// Configurable so tests can run faster.
	segmentPollInterval time.Duration

	// uploadWorker is woken whenever the watcher enqueues a new segment so the
	// upload queue drains promptly. nil is allowed (tests).
	uploadWorker *worker.Upload

	// Active session state (only one recording at a time).
	mu      sync.Mutex
	session *segmentSession
}

// segmentSession tracks one in-flight recording: which segments we've already
// enqueued, the cancel handle for the watcher goroutine, and the recording
// row id so we can link uploads back to it.
type segmentSession struct {
	recordingID int64
	dir         string
	enqueued    map[string]bool
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewRecorder constructs a Recorder. The caller is expected to hand in an
// Execer rooted at the repo root.
func NewRecorder(db *state.DB, exec execsh.Execer, cfg config.Config) *Recorder {
	return &Recorder{
		db:                  db,
		exec:                exec,
		cfg:                 cfg,
		script:              "scripts/record.sh",
		segmentPollInterval: 2 * time.Second,
	}
}

// SetUploadWorker registers the upload worker so the segment watcher can wake
// it after enqueueing. Optional — leaving it unset just means the worker
// drains on its own idle tick instead of immediately.
func (r *Recorder) SetUploadWorker(w *worker.Upload) {
	r.uploadWorker = w
}

// Start invokes `record.sh start [duration_seconds]`, parses stdout to learn
// the ffmpeg pid and session directory, inserts a recordings row, and spawns
// a watcher that enqueues sealed segments as they appear.
//
// EventID is optional; empty = manual recording.
// durationSeconds is optional; 0 = no time cap.
func (r *Recorder) Start(ctx context.Context, eventID string, durationSeconds int) (*state.Recording, error) {
	if err := r.cfg.RequireRecordConfig(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	// Fail early if DB says we're already recording.
	if _, err := r.db.ActiveRecording(ctx); err == nil {
		return nil, errors.New("another recording is already active")
	} else if !errors.Is(err, state.ErrNoActiveRecording) {
		return nil, err
	}

	args := []string{"start"}
	if durationSeconds > 0 {
		args = append(args, strconv.Itoa(durationSeconds))
	}
	res, err := r.exec.RunScript(ctx, r.script, args...)
	if err != nil {
		return nil, fmt.Errorf("exec record.sh start: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("record.sh start exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	pid, sessionDir, err := parseStartOutput(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse record.sh output: %w (stdout=%q)", err, res.Stdout)
	}

	// Log path follows record.sh's convention: same basename as the session
	// directory, .log in the LOG_DIR. Mirrored here so we can present it.
	logPath := mirrorLogPath(sessionDir, r.cfg.LogDir)

	id, err := r.db.StartRecording(ctx, state.NewRecordingInput{
		EventID:       eventID,
		FilePath:      sessionDir,
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

	r.startWatcher(rec.ID, sessionDir)
	return rec, nil
}

// StopResult is what the caller gets back from Stop: the updated recording
// row plus the count of upload rows enqueued during this session.
type StopResult struct {
	Recording *state.Recording
	// UploadID is the id of the *last* upload enqueued during this session, or
	// 0 if no segments were produced. Kept for API compatibility with callers
	// that just want a "did anything get queued" signal.
	UploadID int64
	// UploadIDs is every upload row enqueued for this session, in part order.
	UploadIDs []int64
}

// Stop invokes `record.sh stop` (sends SIGINT to ffmpeg, waits for clean
// finalisation), shuts the segment watcher down, does a final scan to enqueue
// any sealed-but-not-yet-seen parts, and updates the DB.
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
		// still prints the last session dir on stdout. Surface the warning but
		// don't abort: if the DB thought we were recording, there's a row to
		// close.
		if strings.TrimSpace(res.Stdout) == "" {
			return nil, fmt.Errorf("record.sh stop exited %d, no session: %s",
				res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
	sessionDir := strings.TrimSpace(res.Stdout)
	if sessionDir == "" {
		return nil, errors.New("record.sh stop returned no session dir")
	}

	// Shut the watcher down and grab the session handle so we can do a final
	// flush of any segments we hadn't enqueued yet.
	session := r.takeSession()

	rec, err := r.db.StopRecording(ctx, reason)
	if err != nil {
		// If the DB has no active row but the script knew a session, we still
		// want to enqueue any pending parts.
		if !errors.Is(err, state.ErrNoActiveRecording) {
			return nil, fmt.Errorf("record stop update: %w", err)
		}
	}

	finalIDs := r.flushSegments(ctx, session, sessionDir, recordingIDOrZero(rec))

	out := &StopResult{Recording: rec, UploadIDs: finalIDs}
	if len(finalIDs) > 0 {
		out.UploadID = finalIDs[len(finalIDs)-1]
	}
	return out, nil
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

// startWatcher launches the segment-watcher goroutine for an active session.
func (r *Recorder) startWatcher(recordingID int64, dir string) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &segmentSession{
		recordingID: recordingID,
		dir:         dir,
		enqueued:    make(map[string]bool),
		cancel:      cancel,
		done:        make(chan struct{}),
	}

	r.mu.Lock()
	r.session = sess
	r.mu.Unlock()

	go func() {
		defer close(sess.done)
		ticker := time.NewTicker(r.segmentPollInterval)
		defer ticker.Stop()
		for {
			r.scanAndEnqueue(ctx, sess, false)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// takeSession atomically removes and returns the active session, signalling
// the watcher goroutine to exit. Returns nil if no session is active.
func (r *Recorder) takeSession() *segmentSession {
	r.mu.Lock()
	sess := r.session
	r.session = nil
	r.mu.Unlock()
	if sess == nil {
		return nil
	}
	sess.cancel()
	<-sess.done
	return sess
}

// flushSegments runs after Stop: enqueues any remaining sealed parts in the
// session dir. If session is nil (orphan stop), it builds a fresh enqueued-set
// keyed off whatever's already in the uploads table for this dir.
func (r *Recorder) flushSegments(ctx context.Context, session *segmentSession, dir string, recordingID int64) []int64 {
	if session == nil {
		session = &segmentSession{
			recordingID: recordingID,
			dir:         dir,
			enqueued:    make(map[string]bool),
		}
	} else if session.recordingID == 0 && recordingID > 0 {
		session.recordingID = recordingID
	}
	return r.scanAndEnqueue(ctx, session, true)
}

// scanAndEnqueue lists the session dir, decides which parts are sealed, and
// enqueues each new one. When `final` is true, every unenqueued part is
// considered sealed (ffmpeg has exited). When false, only parts whose successor
// already exists are considered sealed — that's the only way we can know a
// segment isn't still being written to.
func (r *Recorder) scanAndEnqueue(ctx context.Context, sess *segmentSession, final bool) []int64 {
	parts, err := listSegments(sess.dir)
	if err != nil {
		// Dir might not exist yet on the very first tick; that's fine.
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("recorder: list segments in %s: %v", sess.dir, err)
		}
		return nil
	}

	var enqueued []int64
	for i, part := range parts {
		if sess.enqueued[part] {
			continue
		}
		// Only enqueue if either (a) we're flushing post-stop, or (b) a later
		// part exists, which means ffmpeg has rotated past this one.
		if !final && i == len(parts)-1 {
			continue
		}
		id, err := r.db.EnqueueUpload(ctx, sess.recordingID, part)
		if err != nil {
			if errors.Is(err, state.ErrUploadExists) {
				// Already enqueued (e.g. by a previous run that crashed mid-stop);
				// remember it so we don't keep retrying.
				sess.enqueued[part] = true
				continue
			}
			log.Printf("recorder: enqueue %s: %v", part, err)
			continue
		}
		sess.enqueued[part] = true
		enqueued = append(enqueued, id)
	}

	if len(enqueued) > 0 && r.uploadWorker != nil {
		r.uploadWorker.Wake()
	}
	return enqueued
}

// listSegments returns the absolute paths of part_NNN.mp4 files in dir, sorted
// numerically by part number.
func listSegments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !partRe.MatchString(name) {
			continue
		}
		parts = append(parts, filepath.Join(dir, name))
	}
	sort.Strings(parts) // zero-padded NNN sorts lexicographically.
	return parts, nil
}

// startOutputRe matches the `recording pid=1234 session=/path/to/session_xxx`
// line record.sh prints on successful start.
var startOutputRe = regexp.MustCompile(`recording\s+pid=(\d+)\s+session=(\S+)`)

// partRe matches the per-segment filename written by ffmpeg's segment muxer.
var partRe = regexp.MustCompile(`^part_\d+\.mp4$`)

func parseStartOutput(stdout string) (pid int, sessionDir string, err error) {
	m := startOutputRe.FindStringSubmatch(stdout)
	if m == nil {
		return 0, "", errors.New("did not find `recording pid=... session=...` line")
	}
	pid, err = strconv.Atoi(m[1])
	if err != nil {
		return 0, "", fmt.Errorf("pid parse: %w", err)
	}
	return pid, m[2], nil
}

// mirrorLogPath reproduces record.sh's convention: the session dir basename
// (e.g. session_20260101_120000) becomes the log basename in LOG_DIR.
func mirrorLogPath(sessionDir, logDir string) string {
	base := filepath.Base(sessionDir)
	return filepath.Join(strings.TrimSuffix(logDir, "/"), base+".log")
}

func recordingIDOrZero(r *state.Recording) int64 {
	if r == nil {
		return 0
	}
	return r.ID
}
