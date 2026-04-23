package tui

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/foundersandcoders/media-pi/internal/state"
)

// tickCmd returns a tea.Cmd that fires a tickMsg after the given duration.
func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// snapshotCmd reads the DB and returns a snapshotMsg. Runs on a goroutine
// under the hood, so it's safe to call from Update.
func snapshotCmd(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		msg := snapshotMsg{now: time.Now().UTC()}

		rec, err := db.ActiveRecording(ctx)
		if err != nil && !errors.Is(err, state.ErrNoActiveRecording) {
			msg.err = err
			return msg
		}
		if rec != nil {
			msg.activeRecording = rec
		}

		recent, err := db.RecentRecordings(ctx, 20)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.recentRecordings = recent

		ups, err := db.ListUploads(ctx, 50)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.uploads = ups

		var beatAt sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT last_beat_at FROM daemon_heartbeat WHERE id = 1`,
		).Scan(&beatAt); err == nil && beatAt.Valid {
			t := time.Unix(beatAt.Int64, 0).UTC()
			msg.daemonHeartbeat = &t
		}
		return msg
	}
}

// tailLogCmd reads the last n lines of a log file and returns a logLinesMsg.
// Cheap enough at 50-100 lines per tick that we don't bother with seeking.
func tailLogCmd(uploadID int64, path string, maxLines int) tea.Cmd {
	return func() tea.Msg {
		msg := logLinesMsg{uploadID: uploadID}
		if path == "" {
			return msg
		}
		f, err := os.Open(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				msg.err = err
			}
			return msg
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if len(lines) > maxLines {
				lines = lines[1:]
			}
		}
		if err := scanner.Err(); err != nil {
			msg.err = err
		}
		msg.lines = lines
		return msg
	}
}
