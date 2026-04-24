package tui

import (
	"time"

	"github.com/foundersandcoders/media-pi/internal/state"
)

// tickMsg fires at TUI_REFRESH_MS to prompt a DB re-read.
type tickMsg time.Time

// snapshotMsg carries a fresh read of everything the dashboard needs.
type snapshotMsg struct {
	now              time.Time
	activeRecording  *state.Recording
	recentRecordings []state.Recording
	uploads          []state.Upload
	daemonHeartbeat  *time.Time
	err              error
}

// logLinesMsg carries the tail of the currently-focused upload's log file.
type logLinesMsg struct {
	uploadID int64
	lines    []string
	err      error
}

// previewExitedMsg fires when the suspended preview process (timg) returns
// control to the TUI. err is whatever the external process produced — nil on
// clean exit (including timg's normal `q` quit), non-nil on spawn failure
// or non-zero exit code. stderr carries whatever timg wrote to stderr so the
// banner / log can surface it; stderr is almost always useful when err != nil.
type previewExitedMsg struct {
	err    error
	stderr string
}
