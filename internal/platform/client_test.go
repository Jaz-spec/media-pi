package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
)

func TestUpcomingEventsHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/pi/pi-1/upcoming-events" {
			http.Error(w, "bad path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"e1","workshop_name":"Algebra","start_time":"2026-04-23T15:00:00Z","end_time":"2026-04-23T16:00:00Z"},
			{"id":"e2","workshop_name":"Calculus","start_time":"2026-04-24T09:00:00Z","end_time":"2026-04-24T10:30:00Z"}
		]`))
	}))
	defer srv.Close()

	c := New(config.Config{
		FACAPIBaseURL: srv.URL,
		FACAPIKey:     "test-key",
		FACPiID:       "pi-1",
	})
	from, _ := time.Parse(time.RFC3339, "2026-04-23T00:00:00Z")
	to := from.Add(48 * time.Hour)

	events, err := c.UpcomingEvents(context.Background(), from, to)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events; got %d", len(events))
	}
	if events[0].ID != "e1" || events[0].WorkshopName != "Algebra" {
		t.Fatalf("wrong event[0]: %+v", events[0])
	}
}

func TestUpcomingEventsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(config.Config{
		FACAPIBaseURL: srv.URL,
		FACAPIKey:     "wrong",
		FACPiID:       "pi-1",
	})
	_, err := c.UpcomingEvents(context.Background(), time.Now(), time.Now().Add(time.Hour))
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("expected ErrAuth; got %v", err)
	}
}

func TestUpcomingEventsFiltersInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Second event has end_time before start_time and should be dropped.
		_, _ = w.Write([]byte(`[
			{"id":"ok","workshop_name":"Good","start_time":"2026-04-23T15:00:00Z","end_time":"2026-04-23T16:00:00Z"},
			{"id":"bad","workshop_name":"Backwards","start_time":"2026-04-23T16:00:00Z","end_time":"2026-04-23T15:00:00Z"},
			{"id":"","workshop_name":"Missing ID","start_time":"2026-04-24T09:00:00Z","end_time":"2026-04-24T10:00:00Z"}
		]`))
	}))
	defer srv.Close()

	c := New(config.Config{
		FACAPIBaseURL: srv.URL,
		FACAPIKey:     "k",
		FACPiID:       "pi-1",
	})
	events, err := c.UpcomingEvents(context.Background(), time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(events) != 1 || events[0].ID != "ok" {
		t.Fatalf("expected only the valid event; got %+v", events)
	}
}

func TestUpcomingEventsMissingConfig(t *testing.T) {
	c := New(config.Config{}) // everything empty
	_, err := c.UpcomingEvents(context.Background(), time.Now(), time.Now())
	if err == nil {
		t.Fatalf("expected config error")
	}
}
