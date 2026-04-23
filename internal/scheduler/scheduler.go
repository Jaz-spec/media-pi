// Package scheduler drives the "fire scheduled recordings" loop: a poller
// that syncs the events cache from the platform API, and a tick that fires
// events whose start_time has arrived.
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/orchestrator"
	"github.com/foundersandcoders/media-pi/internal/platform"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

// Scheduler wires together the poller and the tick.
type Scheduler struct {
	db       *state.DB
	cfg      config.Config
	client   *platform.Client
	recorder *orchestrator.Recorder
	worker   *worker.Upload
}

// New constructs a Scheduler.
func New(db *state.DB, cfg config.Config, client *platform.Client, recorder *orchestrator.Recorder, up *worker.Upload) *Scheduler {
	return &Scheduler{
		db:       db,
		cfg:      cfg,
		client:   client,
		recorder: recorder,
		worker:   up,
	}
}

// Run blocks until ctx is cancelled. Runs both loops concurrently.
func (s *Scheduler) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	go func() { errCh <- s.runPoller(ctx) }()
	go func() { errCh <- s.runTicker(ctx) }()

	// Either loop exiting (other than via cancel) is a fatal scheduler error.
	err := <-errCh
	if errors.Is(err, context.Canceled) {
		return ctx.Err()
	}
	return err
}

// runPoller calls the platform API on an interval and upserts events.
func (s *Scheduler) runPoller(ctx context.Context) error {
	log.Printf("scheduler poller: starting (interval=%s)", s.cfg.SchedulePollInterval)
	// First poll immediately so we don't wait a minute on startup.
	s.pollOnce(ctx)

	ticker := time.NewTicker(s.cfg.SchedulePollInterval)
	defer ticker.Stop()
	authPauseUntil := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().Before(authPauseUntil) {
				continue
			}
			if err := s.pollOnce(ctx); err != nil {
				if errors.Is(err, platform.ErrAuth) {
					log.Printf("scheduler poller: auth error; pausing 5min")
					authPauseUntil = time.Now().Add(5 * time.Minute)
				}
				// Other errors just log; next tick retries.
			}
		}
	}
}

// pollOnce performs a single poll. Separated for testability + early startup
// call. Returns non-nil error for logging / backoff decisions.
func (s *Scheduler) pollOnce(ctx context.Context) error {
	now := time.Now().UTC()
	from := now.Add(-s.cfg.ScheduleLookbehind)
	to := now.Add(s.cfg.ScheduleLookahead)

	events, err := s.client.UpcomingEvents(ctx, from, to)
	if err != nil {
		log.Printf("scheduler poller: %v", err)
		return err
	}
	for _, e := range events {
		// Defensive: don't ingest events whose start time is older than the
		// lookbehind; avoids spurious fires after a long offline period.
		if e.StartTime.Before(from) {
			continue
		}
		if err := s.db.UpsertEvent(ctx, state.Event{
			ID:           e.ID,
			WorkshopName: e.WorkshopName,
			StartTime:    e.StartTime,
			EndTime:      e.EndTime,
		}); err != nil {
			log.Printf("scheduler poller: upsert %s: %v", e.ID, err)
		}
	}

	// Cancel events that have been absent for 2× the poll interval.
	if n, err := s.db.CancelStaleEvents(ctx, 2*s.cfg.SchedulePollInterval); err != nil {
		log.Printf("scheduler poller: cancel stale: %v", err)
	} else if n > 0 {
		log.Printf("scheduler poller: cancelled %d stale events", n)
	}
	return nil
}

// runTicker fires events whose start_time has arrived, and stops events
// whose end_time has arrived.
func (s *Scheduler) runTicker(ctx context.Context) error {
	log.Printf("scheduler tick: starting (every %s)", s.cfg.SchedulerTick)
	ticker := time.NewTicker(s.cfg.SchedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.tickOnce(ctx)
		}
	}
}

func (s *Scheduler) tickOnce(ctx context.Context) {
	now := time.Now().UTC()

	// 1. Stop any fired events whose end_time has passed.
	ending, err := s.db.ScheduledRecordingsEnding(ctx, now)
	if err != nil {
		log.Printf("scheduler tick: lookup ending: %v", err)
	}
	for _, ev := range ending {
		if _, err := s.recorder.Stop(ctx, state.StopReasonScheduledEnd); err != nil {
			if !errors.Is(err, state.ErrNoActiveRecording) {
				log.Printf("scheduler tick: stop event %s: %v", ev.ID, err)
			}
		} else if s.worker != nil {
			s.worker.Wake()
		}
		// Whether or not stop succeeded (e.g. already stopped by the
		// operator) we close the event out so it doesn't keep firing.
		if err := s.db.UpdateEventTrigger(ctx, ev.ID, state.TriggerFired, 0); err != nil {
			log.Printf("scheduler tick: mark event fired: %v", err)
		}
	}

	// 2. Fire any pending events whose start_time has arrived.
	due, err := s.db.DueEvents(ctx, now)
	if err != nil {
		log.Printf("scheduler tick: lookup due: %v", err)
		return
	}
	for _, ev := range due {
		s.fireEvent(ctx, ev)
	}
}

// fireEvent starts a recording for an event. If another recording is already
// active (e.g. manual start picked "no" on interlock), it pre-empts: stops
// the current recording with reason 'operator_override' and starts the
// scheduled one. This matches the interlock design in the plan.
func (s *Scheduler) fireEvent(ctx context.Context, ev state.Event) {
	active, err := s.db.ActiveRecording(ctx)
	if err != nil && !errors.Is(err, state.ErrNoActiveRecording) {
		log.Printf("scheduler fire %s: lookup active: %v", ev.ID, err)
		return
	}
	if active != nil {
		log.Printf("scheduler fire %s: pre-empting active recording id=%d", ev.ID, active.ID)
		if _, err := s.recorder.Stop(ctx, state.StopReasonOperatorOverride); err != nil {
			log.Printf("scheduler fire %s: pre-empt stop failed: %v", ev.ID, err)
			// Don't try to start while there might still be a live ffmpeg.
			return
		}
		if s.worker != nil {
			s.worker.Wake()
		}
	}

	rec, err := s.recorder.Start(ctx, ev.ID)
	if err != nil {
		log.Printf("scheduler fire %s: start failed: %v", ev.ID, err)
		// Mark as missed rather than staying pending (would re-fire forever).
		_ = s.db.UpdateEventTrigger(ctx, ev.ID, state.TriggerMissed, 0)
		return
	}
	if err := s.db.UpdateEventTrigger(ctx, ev.ID, state.TriggerFired, rec.ID); err != nil {
		log.Printf("scheduler fire %s: mark fired: %v", ev.ID, err)
	}
}
