// Package platform talks to the FAC REST API. Only the endpoints the Pi
// needs live here — everything else (watch_ingest_register/confirm) is still
// done by upload-cdn.sh against the GraphQL /g endpoint.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/foundersandcoders/media-pi/internal/config"
)

// Event mirrors state.Event's wire shape. Times are RFC3339 on the wire.
type Event struct {
	ID           string    `json:"id"`
	WorkshopName string    `json:"workshop_name"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
}

// ErrAuth indicates 401/403 from the API. Callers pause polling to avoid
// spinning against a bad key.
var ErrAuth = errors.New("platform: authentication rejected")

// Client is the HTTP client for the platform API.
type Client struct {
	baseURL    string
	apiKey     string
	piID       string
	HTTPClient *http.Client // exposed so tests can swap transports
}

// New constructs a Client. baseURL must be set; apiKey and piID can be
// checked lazily so the TUI-only subcommand doesn't require them.
func New(cfg config.Config) *Client {
	return &Client{
		baseURL: strings.TrimRight(cfg.FACAPIBaseURL, "/"),
		apiKey:  cfg.FACAPIKey,
		piID:    cfg.FACPiID,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// UpcomingEvents calls GET /pi/{pi_id}/upcoming-events?from=<iso>&to=<iso>.
// Returns events with StartTime < EndTime; invalid rows are dropped with a
// warning in the error (as a wrapped error the caller can log).
func (c *Client) UpcomingEvents(ctx context.Context, from, to time.Time) ([]Event, error) {
	if c.baseURL == "" {
		return nil, errors.New("FAC_API_BASE_URL not set")
	}
	if c.piID == "" {
		return nil, errors.New("FAC_PI_ID not set")
	}
	if c.apiKey == "" {
		return nil, errors.New("FAC_API_KEY not set")
	}

	u, err := url.Parse(fmt.Sprintf("%s/pi/%s/upcoming-events", c.baseURL, url.PathEscape(c.piID)))
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}
	q := u.Query()
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u.Path, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read body: %w", readErr)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w (status %d): %s", ErrAuth, resp.StatusCode, trimBody(body))
	default:
		return nil, fmt.Errorf("GET %s: status %d: %s", u.Path, resp.StatusCode, trimBody(body))
	}

	var wire []Event
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode events: %w (body=%s)", err, trimBody(body))
	}

	// Filter obviously invalid events; don't fail the whole poll.
	var out []Event
	for _, e := range wire {
		if e.ID == "" || e.StartTime.IsZero() || e.EndTime.IsZero() {
			continue
		}
		if !e.EndTime.After(e.StartTime) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
