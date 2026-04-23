// Package worker runs background jobs for the daemon. The upload worker
// drains the uploads queue one row at a time, shelling out to upload-cdn.sh
// and recording the outcome in SQLite.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/execsh"
	"github.com/foundersandcoders/media-pi/internal/state"
)

// Upload is the upload-queue worker. One instance per daemon.
type Upload struct {
	db       *state.DB
	exec     execsh.Execer
	cfg      config.Config
	script   string        // relative path to upload-cdn.sh, default "scripts/upload-cdn.sh"
	idleTick time.Duration // wake cadence when queue is empty
	wake     chan struct{}
}

// NewUpload constructs an upload worker. The caller should keep a reference so
// it can call Wake() after enqueueing.
func NewUpload(db *state.DB, exec execsh.Execer, cfg config.Config) *Upload {
	return &Upload{
		db:       db,
		exec:     exec,
		cfg:      cfg,
		script:   "scripts/upload-cdn.sh",
		idleTick: 5 * time.Second,
		// Buffered so non-blocking signals never drop a wake-up.
		wake: make(chan struct{}, 1),
	}
}

// Wake nudges the worker to check the queue now. Safe to call from any
// goroutine; no-op if a wake is already pending.
func (w *Upload) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled, processing the queue serially.
func (w *Upload) Run(ctx context.Context) error {
	log.Printf("upload worker: starting, script=%s", w.script)
	for {
		if err := w.drainOne(ctx); err != nil {
			if errors.Is(err, state.ErrNoPending) || errors.Is(err, context.Canceled) {
				// Nothing to do; fall through to select.
			} else {
				log.Printf("upload worker: drainOne error: %v", err)
			}
		} else {
			// We did some work — loop straight back so we catch anything
			// else that's pending without waiting for a wake.
			continue
		}

		select {
		case <-ctx.Done():
			log.Printf("upload worker: shutting down")
			return ctx.Err()
		case <-w.wake:
		case <-time.After(w.idleTick):
		}
	}
}

// drainOne processes at most one upload. Returns ErrNoPending if queue is
// empty. Any other error is logged but non-fatal to the daemon.
func (w *Upload) drainOne(ctx context.Context) error {
	up, err := w.db.NextPendingUpload(ctx)
	if err != nil {
		return err
	}

	logPath := filepath.Join(w.cfg.LogDir, fmt.Sprintf(
		"upload_%d_%s.log",
		up.ID,
		strings.TrimSuffix(filepath.Base(up.FilePath), filepath.Ext(up.FilePath)),
	))

	attempt, err := w.db.MarkUploadStarted(ctx, up.ID, logPath)
	if err != nil {
		return fmt.Errorf("mark started id=%d: %w", up.ID, err)
	}
	log.Printf("upload worker: id=%d attempt=%d file=%s", up.ID, attempt, up.FilePath)

	exitCode, err := w.exec.StreamScript(ctx, logPath, w.script, up.FilePath)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	} else if exitCode != 0 {
		errMsg = interpretExitCode(exitCode)
	}

	if finErr := w.db.MarkUploadFinished(ctx, up.ID, exitCode, errMsg); finErr != nil {
		// If we can't record the outcome, surface loudly — the upload actually
		// ran, so the DB is the authoritative problem.
		log.Printf("upload worker: FAILED to record finish for id=%d: %v", up.ID, finErr)
		return finErr
	}
	if exitCode == 0 {
		log.Printf("upload worker: id=%d uploaded OK", up.ID)
	} else {
		log.Printf("upload worker: id=%d failed exit=%d: %s", up.ID, exitCode, errMsg)
	}
	return nil
}

// interpretExitCode maps upload-cdn.sh's documented exit codes to human text
// for last_error. Source of truth is the script's header comment.
func interpretExitCode(code int) string {
	switch code {
	case 0:
		return ""
	case 1:
		return "usage or config error"
	case 2:
		return "file missing, empty, or still being recorded"
	case 3:
		return "PUT exhausted retries"
	case 4:
		return "register failed (API unreachable, auth rejected, bad ext)"
	case 5:
		return "confirm failed — file uploaded but row not updated; safe to retry"
	default:
		return fmt.Sprintf("unexpected exit code %d", code)
	}
}
