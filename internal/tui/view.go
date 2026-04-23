package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/foundersandcoders/media-pi/internal/state"
)

// colour palette & styles — modest defaults; we can theme later.
var (
	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86"))

	styleDim = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))

	styleSelected = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("230"))

	stylePane = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
)

// View renders the full TUI.
func (m Model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	header := m.renderHeader()
	body := m.renderBody()
	footer := m.renderFooter()
	base := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	if m.interlock != nil {
		return m.overlayInterlock(base)
	}
	return base
}

func (m Model) overlayInterlock(base string) string {
	title := styleWarn.Render("scheduled event upcoming")
	body := fmt.Sprintf(
		"%s is scheduled to start at %s.\n\n"+
			"Is this recording an early start of that session?\n\n"+
			"  [y] yes — this will subsume %s\n"+
			"  [n] no  — %s will override you at %s\n"+
			"  [esc] dismiss (decide later)",
		styleHeader.Render(m.interlock.eventName),
		styleHeader.Render(m.interlock.eventStart.Local().Format("15:04:05")),
		m.interlock.eventName,
		m.interlock.eventName,
		m.interlock.eventStart.Local().Format("15:04:05"),
	)
	modal := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(1, 2).
		Width(60).
		Render(title + "\n\n" + body)
	// Centre the modal over the existing screen. lipgloss.Place handles the
	// layering for us.
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, modal,
		lipgloss.WithWhitespaceChars(" "))
}

func (m Model) renderHeader() string {
	title := styleTitle.Render("facpi")
	status := "daemon: " + m.daemonStatus()
	if m.active != nil {
		elapsed := time.Since(m.active.StartedAt).Truncate(time.Second)
		status += "   recording: " + styleOK.Render(fmt.Sprintf("●") +
			" " + humanElapsed(elapsed))
	} else {
		status += "   recording: " + styleDim.Render("idle")
	}
	return lipgloss.NewStyle().Padding(0, 1).Render(title + "   " + status)
}

func (m Model) daemonStatus() string {
	if m.daemonHeartbeat == nil {
		return styleWarn.Render("no heartbeat yet")
	}
	age := time.Since(*m.daemonHeartbeat)
	if age < 10*time.Second {
		return styleOK.Render("up")
	}
	return styleErr.Render(fmt.Sprintf("stale (%ds)", int(age.Seconds())))
}

func (m Model) renderBody() string {
	leftW := m.width/2 - 2
	if leftW < 30 {
		leftW = 30
	}
	rightW := m.width - leftW - 4
	if rightW < 30 {
		rightW = 30
	}
	bodyH := m.height - 6
	if bodyH < 10 {
		bodyH = 10
	}

	queue := stylePane.Width(leftW).Height(bodyH).Render(m.renderQueue(leftW - 4))
	logs := stylePane.Width(rightW).Height(bodyH).Render(m.renderLogs(rightW - 4))
	return lipgloss.JoinHorizontal(lipgloss.Top, queue, logs)
}

func (m Model) renderQueue(width int) string {
	var b strings.Builder
	b.WriteString(styleHeader.Render("upload queue"))
	b.WriteString("\n\n")
	if len(m.uploads) == 0 {
		b.WriteString(styleDim.Render("(queue is empty)"))
		return b.String()
	}
	for i, u := range m.uploads {
		line := formatQueueLine(u, width)
		if i == m.selectedIdx {
			line = styleSelected.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func formatQueueLine(u state.Upload, width int) string {
	status := u.Status
	switch u.Status {
	case state.UploadUploaded:
		status = styleOK.Render("✓ " + u.Status)
	case state.UploadFailed:
		status = styleErr.Render("✗ " + u.Status)
	case state.UploadUploading:
		status = styleWarn.Render("↑ " + u.Status)
	case state.UploadPending:
		status = styleDim.Render("• " + u.Status)
	}
	file := shortenPath(u.FilePath, width-30)
	return fmt.Sprintf("%4d  %-24s  %s", u.ID, status, file)
}

func (m Model) renderLogs(width int) string {
	var b strings.Builder
	if sel := m.selectedUploadID(); sel == 0 {
		b.WriteString(styleHeader.Render("log"))
		b.WriteString("\n\n")
		b.WriteString(styleDim.Render("(select an upload to view its log)"))
		return b.String()
	}
	title := fmt.Sprintf("log — upload %d", m.selectedUploadID())
	b.WriteString(styleHeader.Render(title))
	b.WriteString("\n\n")
	if len(m.logLines) == 0 {
		b.WriteString(styleDim.Render("(no log yet)"))
		return b.String()
	}
	// Show the last N lines that fit — the log is already capped by tailLogCmd.
	for _, line := range m.logLines {
		b.WriteString(truncate(line, width))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderFooter() string {
	help := "[r] record   [s] stop   [R] retry failed   [↑/↓] select   [enter] refresh log   [q] quit"
	var lines []string
	if banner := m.activeBanner(); banner != "" {
		lines = append(lines, banner)
	}
	if m.lastErr != nil {
		lines = append(lines, styleErr.Render("err: "+m.lastErr.Error()))
	}
	lines = append(lines, styleDim.Render(help))
	return lipgloss.NewStyle().Padding(0, 1).Render(strings.Join(lines, "\n"))
}

func (m Model) activeBanner() string {
	if m.banner == "" || time.Now().After(m.bannerExpiry) {
		return ""
	}
	switch m.bannerStyle {
	case "ok":
		return styleOK.Render("✓ " + m.banner)
	case "warn":
		return styleWarn.Render("! " + m.banner)
	case "err":
		return styleErr.Render("✗ " + m.banner)
	default:
		return m.banner
	}
}

func humanElapsed(d time.Duration) string {
	s := int(d.Seconds())
	h := s / 3600
	m := (s % 3600) / 60
	s = s % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func shortenPath(p string, width int) string {
	if width <= 0 || len(p) <= width {
		return p
	}
	// keep the tail — the basename is more informative than the prefix
	if width < 4 {
		return p[len(p)-width:]
	}
	return "…" + p[len(p)-(width-1):]
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) <= width {
		return s
	}
	return s[:width-1] + "…"
}
