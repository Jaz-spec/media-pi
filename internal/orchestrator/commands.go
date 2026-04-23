package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/state"
	"github.com/foundersandcoders/media-pi/internal/worker"
)

// CommandConsumer polls the commands table and dispatches each pending row to
// the right handler (recorder or upload queue). One goroutine per daemon.
type CommandConsumer struct {
	db       *state.DB
	recorder *Recorder
	worker   *worker.Upload
	cfg      config.Config
	tick     time.Duration
}

// NewCommandConsumer wires the dependencies.
func NewCommandConsumer(db *state.DB, recorder *Recorder, up *worker.Upload, cfg config.Config) *CommandConsumer {
	return &CommandConsumer{
		db:       db,
		recorder: recorder,
		worker:   up,
		cfg:      cfg,
		tick:     500 * time.Millisecond,
	}
}

// Run blocks until ctx is cancelled.
func (c *CommandConsumer) Run(ctx context.Context) error {
	log.Printf("command consumer: starting (tick=%s)", c.tick)
	ticker := time.NewTicker(c.tick)
	defer ticker.Stop()

	// Drain once immediately so commands inserted during daemon-down are
	// picked up without waiting for the first tick.
	c.drain(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.drain(ctx)
		}
	}
}

func (c *CommandConsumer) drain(ctx context.Context) {
	for {
		cmd, err := c.db.NextPendingCommand(ctx)
		if err != nil {
			if !errors.Is(err, state.ErrNoPending) && !errors.Is(err, context.Canceled) {
				log.Printf("command consumer: pull error: %v", err)
			}
			return
		}
		c.dispatch(ctx, cmd)
	}
}

func (c *CommandConsumer) dispatch(ctx context.Context, cmd *state.Command) {
	log.Printf("command %d: kind=%s", cmd.ID, cmd.Kind)
	switch cmd.Kind {
	case state.CmdStartRecording:
		c.handleStart(ctx, cmd)
	case state.CmdStopRecording:
		c.handleStop(ctx, cmd)
	case state.CmdRetryUpload:
		c.handleRetry(ctx, cmd)
	case state.CmdResolveInterlock:
		c.handleResolveInterlock(ctx, cmd)
	default:
		_ = c.db.MarkCommandError(ctx, cmd.ID,
			jsonErr(fmt.Sprintf("unknown kind %q", cmd.Kind)))
	}
}

func (c *CommandConsumer) handleStart(ctx context.Context, cmd *state.Command) {
	// Parse payload (optional).
	var payload struct {
		EventID        string `json:"event_id"`
		SkipInterlock  bool   `json:"skip_interlock"`
	}
	if cmd.Payload.Valid {
		_ = json.Unmarshal([]byte(cmd.Payload.String), &payload)
	}

	// If the operator has explicitly asked to skip the interlock (via the
	// resolve_interlock "no" path) or this is a scheduled start with a known
	// event, don't re-check.
	if !payload.SkipInterlock && payload.EventID == "" && c.cfg.InterlockWindow > 0 {
		now := time.Now().UTC()
		upcoming, err := c.db.UpcomingEvents(ctx, now, c.cfg.InterlockWindow)
		if err != nil {
			_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("interlock lookup: "+err.Error()))
			return
		}
		if res := CheckInterlock(now, upcoming, c.cfg.InterlockWindow); res.Conflict {
			// Don't start yet — transition to accepted with a result the TUI
			// will pick up and turn into the modal prompt.
			_ = c.db.MarkCommandStatus(ctx, cmd.ID, state.CmdAccepted, mustJSON(map[string]any{
				"kind":        "interlock_required",
				"event_id":    res.Event.ID,
				"event_name":  res.Event.WorkshopName,
				"event_start": res.Event.StartTime.Format(time.RFC3339),
			}))
			return
		}
	}

	rec, err := c.recorder.Start(ctx, payload.EventID)
	if err != nil {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr(err.Error()))
		return
	}
	_ = c.db.MarkCommandDone(ctx, cmd.ID, mustJSON(map[string]any{
		"recording_id": rec.ID,
		"file":         rec.FilePath,
	}))
}

// handleResolveInterlock finalises a start that was paused on an interlock
// prompt. Payload = { "original_cmd_id": int64, "answer": "yes"|"no" }.
func (c *CommandConsumer) handleResolveInterlock(ctx context.Context, cmd *state.Command) {
	if !cmd.Payload.Valid {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("missing payload"))
		return
	}
	var p struct {
		OriginalCmdID int64  `json:"original_cmd_id"`
		Answer        string `json:"answer"`
	}
	if err := json.Unmarshal([]byte(cmd.Payload.String), &p); err != nil {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("bad payload: "+err.Error()))
		return
	}
	if p.OriginalCmdID == 0 || (p.Answer != "yes" && p.Answer != "no") {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("answer must be yes or no"))
		return
	}

	original, err := c.db.GetCommand(ctx, p.OriginalCmdID)
	if err != nil {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("original cmd missing"))
		return
	}
	// Extract the event_id from the original command's result.
	var meta struct {
		EventID   string `json:"event_id"`
		EventName string `json:"event_name"`
	}
	if original.Result.Valid {
		_ = json.Unmarshal([]byte(original.Result.String), &meta)
	}

	switch p.Answer {
	case "yes":
		// Subsume the scheduled event: mark it skipped and start recording
		// linked to it.
		if meta.EventID != "" {
			if err := c.db.UpdateEventTrigger(ctx, meta.EventID, state.TriggerSkippedManual, 0); err != nil {
				log.Printf("resolve interlock yes: mark skipped: %v", err)
			}
		}
		rec, err := c.recorder.Start(ctx, meta.EventID)
		if err != nil {
			_ = c.db.MarkCommandError(ctx, p.OriginalCmdID, jsonErr(err.Error()))
			_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr(err.Error()))
			return
		}
		done := mustJSON(map[string]any{
			"recording_id": rec.ID,
			"file":         rec.FilePath,
			"event_id":     meta.EventID,
			"interlock":    "skipped_manual",
		})
		_ = c.db.MarkCommandDone(ctx, p.OriginalCmdID, done)
		_ = c.db.MarkCommandDone(ctx, cmd.ID, done)

	case "no":
		// Start without touching the event. The scheduler will pre-empt at
		// start_time.
		rec, err := c.recorder.Start(ctx, "")
		if err != nil {
			_ = c.db.MarkCommandError(ctx, p.OriginalCmdID, jsonErr(err.Error()))
			_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr(err.Error()))
			return
		}
		done := mustJSON(map[string]any{
			"recording_id": rec.ID,
			"file":         rec.FilePath,
			"interlock":    "will_be_overridden",
		})
		_ = c.db.MarkCommandDone(ctx, p.OriginalCmdID, done)
		_ = c.db.MarkCommandDone(ctx, cmd.ID, done)
	}
}

func (c *CommandConsumer) handleStop(ctx context.Context, cmd *state.Command) {
	reason := state.StopReasonManual
	if cmd.Payload.Valid {
		var p struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(cmd.Payload.String), &p); err == nil && p.Reason != "" {
			reason = p.Reason
		}
	}
	res, err := c.recorder.Stop(ctx, reason)
	if err != nil {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr(err.Error()))
		return
	}
	if res.UploadID > 0 && c.worker != nil {
		c.worker.Wake()
	}
	out := map[string]any{}
	if res.Recording != nil {
		out["recording_id"] = res.Recording.ID
	}
	if res.UploadID > 0 {
		out["upload_id"] = res.UploadID
	}
	_ = c.db.MarkCommandDone(ctx, cmd.ID, mustJSON(out))
}

func (c *CommandConsumer) handleRetry(ctx context.Context, cmd *state.Command) {
	var p struct {
		UploadID int64 `json:"upload_id"`
	}
	if !cmd.Payload.Valid {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("retry_upload: missing payload"))
		return
	}
	if err := json.Unmarshal([]byte(cmd.Payload.String), &p); err != nil {
		// Accept the legacy "raw id" shape for ergonomics.
		if n, perr := strconv.ParseInt(cmd.Payload.String, 10, 64); perr == nil {
			p.UploadID = n
		} else {
			_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr("retry_upload: bad payload"))
			return
		}
	}
	if err := c.db.RetryUpload(ctx, p.UploadID); err != nil {
		_ = c.db.MarkCommandError(ctx, cmd.ID, jsonErr(err.Error()))
		return
	}
	if c.worker != nil {
		c.worker.Wake()
	}
	_ = c.db.MarkCommandDone(ctx, cmd.ID, mustJSON(map[string]any{
		"upload_id": p.UploadID,
	}))
}

func jsonErr(msg string) string { return mustJSON(map[string]string{"error": msg}) }

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
