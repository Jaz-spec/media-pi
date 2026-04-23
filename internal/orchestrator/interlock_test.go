package orchestrator

import (
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/state"
)

func TestCheckInterlockNoEvents(t *testing.T) {
	now := time.Now()
	res := CheckInterlock(now, nil, 30*time.Minute)
	if res.Conflict {
		t.Fatalf("expected no conflict with no events")
	}
}

func TestCheckInterlockNearbyEvent(t *testing.T) {
	now := time.Now()
	events := []state.Event{{
		ID:            "e1",
		WorkshopName:  "Algebra",
		StartTime:     now.Add(15 * time.Minute),
		EndTime:       now.Add(75 * time.Minute),
		TriggerStatus: state.TriggerPending,
	}}
	res := CheckInterlock(now, events, 30*time.Minute)
	if !res.Conflict {
		t.Fatalf("expected conflict with event 15min away, window 30min")
	}
	if res.Event == nil || res.Event.ID != "e1" {
		t.Fatalf("wrong conflict event: %+v", res.Event)
	}
}

func TestCheckInterlockOutsideWindow(t *testing.T) {
	now := time.Now()
	events := []state.Event{{
		ID:            "e1",
		StartTime:     now.Add(2 * time.Hour),
		EndTime:       now.Add(3 * time.Hour),
		TriggerStatus: state.TriggerPending,
	}}
	res := CheckInterlock(now, events, 30*time.Minute)
	if res.Conflict {
		t.Fatalf("expected no conflict — event outside window")
	}
}

func TestCheckInterlockIgnoresAlreadyStarted(t *testing.T) {
	now := time.Now()
	events := []state.Event{{
		ID:            "past",
		StartTime:     now.Add(-1 * time.Minute),
		EndTime:       now.Add(30 * time.Minute),
		TriggerStatus: state.TriggerPending,
	}}
	res := CheckInterlock(now, events, 30*time.Minute)
	if res.Conflict {
		t.Fatalf("expected no conflict — event already started")
	}
}

func TestCheckInterlockIgnoresNonPending(t *testing.T) {
	now := time.Now()
	events := []state.Event{{
		ID:            "e1",
		StartTime:     now.Add(10 * time.Minute),
		EndTime:       now.Add(60 * time.Minute),
		TriggerStatus: state.TriggerSkippedManual, // already resolved
	}}
	res := CheckInterlock(now, events, 30*time.Minute)
	if res.Conflict {
		t.Fatalf("expected no conflict for non-pending event")
	}
}

func TestCheckInterlockPicksNearest(t *testing.T) {
	now := time.Now()
	events := []state.Event{
		{
			ID:            "further",
			StartTime:     now.Add(25 * time.Minute),
			EndTime:       now.Add(90 * time.Minute),
			TriggerStatus: state.TriggerPending,
		},
		{
			ID:            "nearer",
			StartTime:     now.Add(10 * time.Minute),
			EndTime:       now.Add(40 * time.Minute),
			TriggerStatus: state.TriggerPending,
		},
	}
	res := CheckInterlock(now, events, 30*time.Minute)
	if !res.Conflict {
		t.Fatalf("expected conflict")
	}
	if res.Event.ID != "nearer" {
		t.Fatalf("expected nearest event; got %s", res.Event.ID)
	}
}
