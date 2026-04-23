package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/state"
)

// Run opens the TUI and blocks until the user quits. The database at
// cfg.DBPath must already exist and have its schema applied (the daemon
// runs migrations).
//
// The TUI opens the DB writable in order to insert rows into the `commands`
// table — its only write path. All business-logic state transitions
// (recordings, uploads, events) remain exclusively the daemon's. WAL mode
// plus a 5s busy_timeout means the TUI and daemon coexist cleanly.
func Run(cfg config.Config) error {
	db, err := state.Open(cfg.DBPath, false)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	p := tea.NewProgram(NewModel(db, cfg), tea.WithAltScreen())
	_, err = p.Run()
	return err
}
