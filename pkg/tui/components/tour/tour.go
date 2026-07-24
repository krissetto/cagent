// Package tour implements the interactive getting-started tour: a floating
// card that teaches docker agent by doing. Each step waits for the user to
// actually perform the action it describes (send a message, approve a tool
// call, open the palette…) and observes the TUI's message stream to detect
// completion. The card never steals focus: the user drives the real UI.
package tour

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// celebrationDelay is how long the "step done" flash stays on screen
	// before the tour advances to the next step on its own.
	celebrationDelay = 1500 * time.Millisecond

	maxCardWidth = 46
)

// advanceMsg moves the tour past a completed step once its celebration flash
// has been shown. The step index guards against stale ticks after a manual
// skip already advanced the tour.
type advanceMsg struct{ step int }

// step is one stop of the tour. A step with a check function completes when
// the check matches an observed message; a step without one is a read step
// that the user advances manually (Enter).
type step struct {
	title     string
	body      string
	action    string
	check     func(tea.Msg) bool
	celebrate string
}

// Model is the tour engine plus its floating card. The zero value is
// inactive; use New.
type Model struct {
	steps         []step
	idx           int
	active        bool
	celebrating   bool
	width, height int
	contentBottom int
}

// New returns an inactive tour with the built-in steps.
func New() *Model {
	return &Model{steps: buildSteps()}
}

// Active reports whether the tour is currently running.
func (m *Model) Active() bool {
	return m != nil && m.active
}

// Start (re)starts the tour from the first step.
func (m *Model) Start() {
	m.idx = 0
	m.celebrating = false
	m.active = true
}

// Quit stops the tour early and reports it as not completed.
func (m *Model) Quit() tea.Cmd {
	if !m.Active() {
		return nil
	}
	m.active = false
	return core.CmdHandler(messages.TourFinishedMsg{Completed: false})
}

// SetSize records the window size and the row where the content area ends
// (just above the resize handle), both used to position the card.
func (m *Model) SetSize(width, height, contentBottom int) {
	if m == nil {
		return
	}
	m.width, m.height = width, height
	m.contentBottom = contentBottom
}

// Observe inspects a message flowing through the TUI and completes the
// current step when the user performed its action. It never consumes the
// message; the returned command only drives the tour's own progression.
func (m *Model) Observe(msg tea.Msg) tea.Cmd {
	if !m.Active() {
		return nil
	}

	if adv, ok := msg.(advanceMsg); ok {
		if m.celebrating && adv.step == m.idx {
			return m.next()
		}
		return nil
	}

	if m.celebrating {
		return nil
	}
	current := m.steps[m.idx]
	if current.check == nil || !current.check(msg) {
		return nil
	}

	m.celebrating = true
	stepIdx := m.idx
	return tea.Tick(celebrationDelay, func(time.Time) tea.Msg {
		return advanceMsg{step: stepIdx}
	})
}

// Advance is the manual progression driven by the Enter key: it skips the
// celebration wait, moves past a read step, or skips an action step the user
// does not want to perform.
func (m *Model) Advance() tea.Cmd {
	if !m.Active() {
		return nil
	}
	return m.next()
}

// next moves to the following step, ending the tour after the last one.
func (m *Model) next() tea.Cmd {
	m.celebrating = false
	if m.idx >= len(m.steps)-1 {
		m.active = false
		return core.CmdHandler(messages.TourFinishedMsg{Completed: true})
	}
	m.idx++
	return nil
}

// OnLastStep reports whether the tour is showing its final step.
func (m *Model) OnLastStep() bool {
	return m.Active() && m.idx == len(m.steps)-1
}

// Layer returns the floating card as a lipgloss layer anchored to the
// bottom-right of the content area, below the sidebar's agent info and
// tools so it hides as little as possible. Nil when the tour is inactive.
func (m *Model) Layer() *lipgloss.Layer {
	if !m.Active() || m.width < 20 || m.height < 10 {
		return nil
	}

	view := m.view()
	col := max(1, m.width-lipgloss.Width(view)-1)
	row := max(1, m.contentBottom-lipgloss.Height(view))
	return lipgloss.NewLayer(view).X(col).Y(row)
}

func (m *Model) view() string {
	cardWidth := min(maxCardWidth, m.width-4)
	frame := styles.DialogStyle.
		BorderForeground(styles.Highlight).
		Padding(1, 2).
		Width(cardWidth)
	contentWidth := cardWidth - frame.GetHorizontalFrameSize()

	var body string
	if m.celebrating {
		body = m.celebrationView(contentWidth)
	} else {
		body = m.stepView(contentWidth)
	}

	header := lipgloss.JoinHorizontal(lipgloss.Top,
		styles.BoldStyle.Foreground(styles.Highlight).Render("Getting started"),
		lipgloss.PlaceHorizontal(
			max(0, contentWidth-lipgloss.Width("Getting started")),
			lipgloss.Right,
			styles.MutedStyle.Render(m.progressLabel()),
		),
	)

	content := lipgloss.JoinVertical(lipgloss.Left,
		header,
		styles.DialogSeparatorStyle.Render(strings.Repeat("─", max(1, contentWidth))),
		"",
		body,
		"",
		m.footer(contentWidth),
	)

	return frame.Render(content)
}

func (m *Model) stepView(contentWidth int) string {
	current := m.steps[m.idx]

	parts := []string{
		styles.HighlightWhiteStyle.Width(contentWidth).Render(current.title),
		"",
		styles.SecondaryStyle.Width(contentWidth).Render(current.body),
	}
	if current.action != "" {
		parts = append(parts,
			"",
			lipgloss.JoinHorizontal(lipgloss.Top,
				styles.BaseStyle.Foreground(styles.Highlight).Render("▶ "),
				styles.BaseStyle.Width(max(1, contentWidth-2)).Render(current.action),
			),
		)
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *Model) celebrationView(contentWidth int) string {
	return lipgloss.JoinVertical(lipgloss.Left,
		styles.SuccessStyle.Bold(true).Width(contentWidth).Render("✔ "+m.steps[m.idx].celebrate),
		"",
		styles.MutedStyle.Width(contentWidth).Render("Next stop in a moment… (↵ to jump)"),
	)
}

// progressLabel renders one dot per step: filled for done, target glyph for
// the current step, hollow for what's left.
func (m *Model) progressLabel() string {
	var b strings.Builder
	for i := range m.steps {
		switch {
		case i < m.idx, i == m.idx && m.celebrating:
			b.WriteString("●")
		case i == m.idx:
			b.WriteString("◉")
		default:
			b.WriteString("○")
		}
	}
	return b.String()
}

func (m *Model) footer(contentWidth int) string {
	enterHint := "skip step"
	switch {
	case m.OnLastStep():
		enterHint = "finish"
	case m.celebrating || m.steps[m.idx].check == nil:
		enterHint = "continue"
	}

	keyStyle := styles.HighlightWhiteStyle
	descStyle := styles.MutedStyle
	line := keyStyle.Render("↵") + descStyle.Render(" "+enterHint)
	return lipgloss.PlaceHorizontal(max(1, contentWidth), lipgloss.Right, line)
}
