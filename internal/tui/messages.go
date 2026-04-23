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
