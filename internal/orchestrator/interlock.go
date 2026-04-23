package orchestrator

import (
	"time"

	"github.com/foundersandcoders/media-pi/internal/state"
)

// InterlockResult tells the start-recording command handler what to do when
// a manual start comes in. Conflict=false means no scheduled event is near
// enough to matter; proceed with start. Conflict=true means the TUI must
// prompt the operator.
type InterlockResult struct {
	Conflict bool
	Event    *state.Event
}

// CheckInterlock finds the nearest upcoming pending event within the window
// [now, now+window]. If one exists, a manual start must prompt the operator.
// Pure function so it's trivially testable.
func CheckInterlock(now time.Time, upcoming []state.Event, window time.Duration) InterlockResult {
	threshold := now.Add(window)
	var best *state.Event
	for i := range upcoming {
		ev := upcoming[i]
		if ev.TriggerStatus != state.TriggerPending {
			continue
		}
		// Must be strictly future (if it's already started, it's not
		// a "scheduled start coming up").
		if !ev.StartTime.After(now) {
			continue
		}
		if ev.StartTime.After(threshold) {
			continue
		}
		if best == nil || ev.StartTime.Before(best.StartTime) {
			best = &ev
		}
	}
	if best == nil {
		return InterlockResult{Conflict: false}
	}
	return InterlockResult{Conflict: true, Event: best}
}
