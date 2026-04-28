package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/foundersandcoders/media-pi/internal/config"
	"github.com/foundersandcoders/media-pi/internal/state"
)

// previewDevice is the v4l2 path the preview tool opens. Centralised here so
// we can lift it into Config later if a Pi ever uses something other than
// /dev/video0 (none currently do).
const previewDevice = "/dev/video0"

// Model is the root Bubble Tea model. One per running TUI.
type Model struct {
	db  *state.DB
	cfg config.Config

	// Last snapshot from the DB.
	now             time.Time
	active          *state.Recording
	recent          []state.Recording
	uploads         []state.Upload
	daemonHeartbeat *time.Time
	lastErr         error

	// UI state.
	width, height int
	selectedIdx   int // index into uploads for the queue pane
	logLines      []string
	logUploadID   int64 // which upload the logLines belong to

	// Transient banner messages — shown in the footer for a few seconds.
	banner       string
	bannerStyle  string // "info"|"ok"|"warn"|"err"
	bannerExpiry time.Time

	// Interlock modal state. Non-nil means we are displaying the modal.
	interlock *interlockState

	// Last start_recording command we emitted; we watch its result to see
	// if the daemon needs an interlock resolution.
	watchedStartCmdID int64
}

// interlockState captures everything the modal needs to render itself and
// build the resolve_interlock command on answer.
type interlockState struct {
	originalCmdID int64
	eventID       string
	eventName     string
	eventStart    time.Time
}

// NewModel constructs the starting model.
func NewModel(db *state.DB, cfg config.Config) Model {
	return Model{db: db, cfg: cfg}
}

// Init is called once at startup. We kick off the first snapshot and tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(snapshotCmd(m.db), tickCmd(m.cfg.TUIRefresh))
}

// Update handles all incoming messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		// If the interlock modal is up, routes are restricted.
		if m.interlock != nil {
			switch msg.String() {
			case "y", "Y":
				m = m.resolveInterlock("yes")
				return m, nil
			case "n", "N":
				m = m.resolveInterlock("no")
				return m, nil
			case "esc":
				// Dismiss without resolving — the start command stays
				// accepted on the daemon side until the operator re-answers.
				m.interlock = nil
				m.setBanner("warn", "interlock dismissed — re-press 'r' to resolve")
				return m, nil
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.selectedIdx > 0 {
				m.selectedIdx--
			}
			return m, m.currentLogTailCmd()
		case "down", "j":
			if m.selectedIdx < len(m.uploads)-1 {
				m.selectedIdx++
			}
			return m, m.currentLogTailCmd()
		case "enter":
			return m, m.currentLogTailCmd()
		case "r":
			id, ok := m.emitCommandID(state.CmdStartRecording, "", "recording requested")
			if ok {
				m.watchedStartCmdID = id
			}
			return m, nil
		case "s":
			_, _ = m.emitCommandID(state.CmdStopRecording, "", "stop requested")
			return m, nil
		case "R":
			u := m.selectedUpload()
			if u == nil || u.Status != state.UploadFailed {
				m.setBanner("warn", "select a failed upload first")
				return m, nil
			}
			payload := fmt.Sprintf(`{"upload_id":%d}`, u.ID)
			_, _ = m.emitCommandID(state.CmdRetryUpload, payload,
				fmt.Sprintf("retry requested for id=%d", u.ID))
			return m, nil
		case "f":
			cmd, banner := m.previewCmd()
			if banner.style != "" {
				m.setBanner(banner.style, banner.msg)
			}
			return m, cmd
		}

	case tickMsg:
		return m, tea.Batch(snapshotCmd(m.db), tickCmd(m.cfg.TUIRefresh))

	case snapshotMsg:
		m.now = msg.now
		m.active = msg.activeRecording
		m.recent = msg.recentRecordings
		m.uploads = msg.uploads
		m.daemonHeartbeat = msg.daemonHeartbeat
		m.lastErr = msg.err
		// Clamp selection if queue shrank.
		if m.selectedIdx >= len(m.uploads) {
			m.selectedIdx = max0(len(m.uploads) - 1)
		}
		// Check if our watched start_recording command needs the modal.
		m = m.pollWatchedStart()
		// Re-tail the selected row's log in case it rotated.
		return m, m.currentLogTailCmd()

	case logLinesMsg:
		if msg.err != nil {
			m.lastErr = msg.err
		}
		// Only accept lines for the currently-selected upload.
		if msg.uploadID == m.selectedUploadID() {
			m.logLines = msg.lines
			m.logUploadID = msg.uploadID
		}

	case previewExitedMsg:
		if msg.err != nil {
			m.setBanner("err", "preview exited: "+msg.err.Error())
		} else {
			m.setBanner("ok", "preview ended")
		}
		// Force an immediate snapshot — the camera may now be free for
		// recording, and the user expects responsive feedback.
		return m, snapshotCmd(m.db)
	}
	return m, nil
}

// selectedUploadID returns the id of the upload at selectedIdx, or 0 if
// there are no uploads.
func (m Model) selectedUploadID() int64 {
	if len(m.uploads) == 0 || m.selectedIdx < 0 || m.selectedIdx >= len(m.uploads) {
		return 0
	}
	return m.uploads[m.selectedIdx].ID
}

// currentLogTailCmd returns a tea.Cmd that tails the selected upload's log
// file. nil if nothing is selected or the row has no log path.
func (m Model) currentLogTailCmd() tea.Cmd {
	if len(m.uploads) == 0 {
		return nil
	}
	u := m.uploads[m.selectedIdx]
	if !u.LastLogPath.Valid || u.LastLogPath.String == "" {
		return nil
	}
	return tailLogCmd(u.ID, u.LastLogPath.String, 200)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// selectedUpload returns a pointer to the currently-selected Upload, or nil.
func (m Model) selectedUpload() *state.Upload {
	if len(m.uploads) == 0 || m.selectedIdx < 0 || m.selectedIdx >= len(m.uploads) {
		return nil
	}
	u := m.uploads[m.selectedIdx]
	return &u
}

// emitCommandID inserts a command row, sets a banner, and returns the id.
// The second return is false if insert failed (caller can skip further work).
func (m Model) emitCommandID(kind, payload, okMsg string) (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	id, err := m.db.InsertCommand(ctx, kind, payload)
	if err != nil {
		m.setBanner("err", "cmd insert failed: "+err.Error())
		return 0, false
	}
	if payload != "" {
		var probe any
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			m.setBanner("warn", "payload not json; daemon may reject")
			return id, true
		}
	}
	m.setBanner("ok", okMsg)
	return id, true
}

// pollWatchedStart checks whether the currently-watched start_recording
// command has transitioned to the interlock-required state and, if so,
// populates m.interlock. Called on every snapshot.
func (m Model) pollWatchedStart() Model {
	if m.watchedStartCmdID == 0 {
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	cmd, err := m.db.GetCommand(ctx, m.watchedStartCmdID)
	if err != nil {
		return m
	}
	switch cmd.Status {
	case state.CmdAccepted:
		if cmd.Result.Valid {
			var r struct {
				Kind       string `json:"kind"`
				EventID    string `json:"event_id"`
				EventName  string `json:"event_name"`
				EventStart string `json:"event_start"`
			}
			if err := json.Unmarshal([]byte(cmd.Result.String), &r); err == nil && r.Kind == "interlock_required" {
				start, _ := time.Parse(time.RFC3339, r.EventStart)
				m.interlock = &interlockState{
					originalCmdID: cmd.ID,
					eventID:       r.EventID,
					eventName:     r.EventName,
					eventStart:    start,
				}
			}
		}
	case state.CmdDone, state.CmdError:
		// Command finalised — stop watching.
		m.watchedStartCmdID = 0
		m.interlock = nil
	}
	return m
}

// resolveInterlock inserts a resolve_interlock command and clears the modal.
func (m Model) resolveInterlock(answer string) Model {
	if m.interlock == nil {
		return m
	}
	payload := fmt.Sprintf(`{"original_cmd_id":%d,"answer":%q}`, m.interlock.originalCmdID, answer)
	_, _ = m.emitCommandID(state.CmdResolveInterlock, payload,
		fmt.Sprintf("interlock: %s", answer))
	m.interlock = nil
	// Keep watchedStartCmdID set so pollWatchedStart sees it reach Done.
	return m
}

func (m *Model) setBanner(style, msg string) {
	m.banner = msg
	m.bannerStyle = style
	m.bannerExpiry = time.Now().Add(4 * time.Second)
}

// bannerHint is a tiny carrier for "set this banner" returned alongside a
// tea.Cmd, so the caller can update the model AND fire a command in one go.
type bannerHint struct {
	style string
	msg   string
}

// previewCmd builds the tea.Cmd that suspends the TUI and runs `timg`
// against /dev/video0. Returns nil + a banner if preview can't run right now
// (timg missing, recording active, or DB lookup failed).
func (m Model) previewCmd() (tea.Cmd, bannerHint) {
	// Refuse early if timg isn't on PATH — friendlier than a cryptic
	// "exec: timg: not found" surfacing through previewExitedMsg.
	if _, err := exec.LookPath("timg"); err != nil {
		return nil, bannerHint{
			style: "warn",
			msg:   "preview tool not installed: sudo apt install timg",
		}
	}

	// Camera-busy guard: don't even try if the daemon's recording. ffmpeg
	// would block our v4l2 open with EBUSY anyway, but this gives the
	// operator a clear message instead of timg flashing an error.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if active, err := m.db.ActiveRecording(ctx); err == nil && active != nil {
		return nil, bannerHint{
			style: "warn",
			msg:   "stop recording before previewing",
		}
	} else if err != nil && !errors.Is(err, state.ErrNoActiveRecording) {
		return nil, bannerHint{
			style: "err",
			msg:   "couldn't check recording state: " + err.Error(),
		}
	}

	// `-V` puts timg in video mode (treat input as a stream). `--frame-rate`
	// caps the refresh rate to keep CPU + SSH bandwidth modest. Block-char
	// rendering picks the cell-doubling glyphs automatically.
	cmd := exec.Command("timg", "-V", previewDevice)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return previewExitedMsg{err: err}
	}), bannerHint{}
}
