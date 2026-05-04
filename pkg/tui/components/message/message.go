package message

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Model represents a view that can render a message
type Model interface {
	layout.Model
	layout.Sizeable
	SetMessage(msg *types.Message)
	SetSelected(selected bool)
	SetHovered(hovered bool)
}

// messageModel implements Model
type messageModel struct {
	message      *types.Message
	previous     *types.Message
	sessionState service.SessionStateReader

	width    int
	height   int
	focused  bool
	selected bool
	hovered  bool
	spinner  spinner.Spinner
}

// New creates a new message view
func New(msg, previous *types.Message, sessionState service.SessionStateReader) *messageModel {
	return &messageModel{
		message:      msg,
		previous:     previous,
		sessionState: sessionState,
		width:        80, // Default width
		height:       1,  // Will be calculated
		focused:      false,
		spinner:      spinner.New(spinner.ModeBoth, styles.SpinnerDotsAccentStyle),
	}
}

// Bubble Tea Model methods

// Init initializes the message view
func (mv *messageModel) Init() tea.Cmd {
	if mv.message.Type == types.MessageTypeSpinner || mv.message.Type == types.MessageTypeLoading {
		return mv.spinner.Init()
	}
	return nil
}

func (mv *messageModel) SetMessage(msg *types.Message) {
	mv.message = msg
}

func (mv *messageModel) SetSelected(selected bool) {
	mv.selected = selected
}

func (mv *messageModel) SetHovered(hovered bool) {
	mv.hovered = hovered
}

// Update handles messages and updates the message view state
func (mv *messageModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if mv.message.Type == types.MessageTypeSpinner || mv.message.Type == types.MessageTypeLoading {
		s, cmd := mv.spinner.Update(msg)
		mv.spinner = s.(spinner.Spinner)
		return mv, cmd
	}
	return mv, nil
}

// View renders the message view
func (mv *messageModel) View() string {
	return mv.Render(mv.width)
}

// Render renders the message view content
func (mv *messageModel) Render(width int) string {
	msg := mv.message
	switch msg.Type {
	case types.MessageTypeSpinner:
		return mv.spinner.View()
	case types.MessageTypeUser:
		// Choose style based on selection state
		messageStyle := styles.UserMessageStyle
		if mv.selected && msg.SessionPosition != nil {
			messageStyle = styles.SelectedUserMessageStyle
		}

		if msg.SessionPosition == nil {
			return messageStyle.Width(width).Render(msg.Content)
		}

		// For editable messages, place the pencil icon in the top padding row
		innerWidth := width - messageStyle.GetHorizontalFrameSize()
		content := strings.TrimRight(msg.Content, "\n\r\t ")
		if content == "" {
			content = msg.Content
		}

		// Create the edit icon for the top row
		editIcon := styles.MutedStyle.Render(types.UserMessageEditLabel)
		iconWidth := ansi.StringWidth(types.UserMessageEditLabel)

		// Create a top row with the icon pushed to the right edge
		// This row replaces the top padding and becomes part of the content
		topPadding := max(innerWidth-iconWidth, 0)
		topRow := strings.Repeat(" ", topPadding) + editIcon

		// Combine: icon row + content (icon row acts as the top padding)
		contentWithIcon := topRow + "\n" + content

		// Use a modified style with no top padding (our icon row replaces it)
		noTopPaddingStyle := messageStyle.PaddingTop(0)
		return noTopPaddingStyle.Width(width).Render(contentWithIcon)
	case types.MessageTypeAssistant:
		if msg.Content == "" {
			return mv.spinner.View()
		}

		messageStyle := styles.AssistantMessageStyle
		if mv.selected {
			messageStyle = styles.SelectedMessageStyle
		}

		rendered, err := markdown.NewRenderer(width - messageStyle.GetHorizontalFrameSize()).Render(msg.Content)
		if err != nil {
			rendered = msg.Content
		}

		// The per-message sender pill was intentionally removed: the sidebar's
		// "selected agent" indicator + working spinner are the canonical places
		// to look for agent attribution. Showing the pill inconsistently here —
		// only when the turn opened with assistant text rather than a tool call
		// or subagent delegation — was confusing in multi-agent sessions.

		// Always reserve a top row to avoid layout shifts when the copy icon
		// appears on hover. When not hovered, the row is filled with spaces
		// (invisible). AssistantMessageStyle has PaddingTop=0, so this extra
		// row acts as a stable spacer.
		innerWidth := width - messageStyle.GetHorizontalFrameSize()
		topRow := strings.Repeat(" ", innerWidth)
		if mv.hovered || mv.selected {
			copyIcon := styles.MutedStyle.Render(types.AssistantMessageCopyLabel)
			iconWidth := ansi.StringWidth(types.AssistantMessageCopyLabel)
			padding := max(innerWidth-iconWidth, 0)
			topRow = strings.Repeat(" ", padding) + copyIcon
		}
		return messageStyle.Width(width).Render(topRow + "\n" + rendered)
	case types.MessageTypeShellOutput:
		if rendered, err := markdown.NewRenderer(width).Render(fmt.Sprintf("```console\n%s\n```", msg.Content)); err == nil {
			return rendered
		}
		return msg.Content
	case types.MessageTypeCancelled:
		return styles.WarningStyle.Render("⚠ stream cancelled ⚠")
	case types.MessageTypeWelcome:
		messageStyle := styles.WelcomeMessageStyle
		// Convert explicit newlines to markdown hard line breaks (two trailing spaces)
		// This preserves line breaks from YAML multiline syntax (|) while still
		// allowing markdown formatting like **bold** and *italic*
		content := preserveLineBreaks(msg.Content)
		rendered, err := markdown.NewRenderer(width - messageStyle.GetHorizontalFrameSize()).Render(content)
		if err != nil {
			rendered = msg.Content
		}
		return messageStyle.Width(width - 1).Render(strings.TrimRight(rendered, "\n\r\t "))
	case types.MessageTypeError:
		return styles.ErrorMessageStyle.Width(width - 1).Render(msg.Content)
	case types.MessageTypeLoading:
		// Show spinner with the loading description, truncated to fit width
		spinnerView := mv.spinner.View()
		spinnerWidth := ansi.StringWidth(spinnerView) + 1 // +1 for space separator
		maxDescWidth := width - spinnerWidth
		description := msg.Content
		if maxDescWidth > 0 && ansi.StringWidth(description) > maxDescWidth {
			description = ansi.Truncate(description, maxDescWidth, "…")
		}
		return spinnerView + " " + styles.MutedStyle.Render(description)
	case types.MessageTypeSubAgent:
		if msg.SubAgent == nil {
			return styles.MutedStyle.Render("Subagent event")
		}
		return mv.renderSubAgent(width, msg.SubAgent)
	default:
		return msg.Content
	}
}

func (mv *messageModel) renderSubAgent(width int, info *types.SubAgentInfo) string {
	agent := strings.TrimSpace(info.AgentName)
	header := ""
	if agent != "" {
		header = styles.AgentBadgeStyleFor(agent).Render(agent)
	}
	if info.ShortID != "" {
		idPart := styles.MutedStyle.Render("(" + info.ShortID + ")")
		if header == "" {
			header = idPart
		} else {
			header += " " + idPart
		}
	}

	// Glyph + trailing copy for each lifecycle kind. Turn-completed cards
	// reuse the same ✓ status icon as completed tool rows so they read as
	// part of the same cadence in the transcript instead of looking like a
	// distinct message class.
	var (
		glyph   string
		trailer string
	)
	switch info.Kind {
	case types.SubAgentEventTurnCompleted:
		// Turn-finished is a notification from a subagent, not a tool result.
		// We use a plain '<' so the row reads as "incoming from a subagent"
		// (mirroring the agent-section's '▶'), and because plain ASCII has
		// guaranteed single-cell width across every monospace font / terminal
		// combination, unlike fancier glyphs whose dimensions vary.
		glyph = "<"
		trailer = "turn finished"
	case types.SubAgentEventClosed:
		glyph = "◇"
		trailer = "finalized"
	case types.SubAgentEventStopped:
		glyph = "■"
		trailer = "stopped"
	case types.SubAgentEventFailed:
		glyph = "!"
		trailer = "failed"
	default:
		glyph = "→"
	}

	// Render the leading glyph through the same icon style as completed tool
	// rows. ToolCompletedIcon contributes the canonical MarginLeft(2) gutter,
	// so subagent lifecycle cards line up column-for-column with adjacent
	// `✓ inspecting ...` / `✓ replying to ...` tool rows.
	line := styles.ToolCompletedIcon.Render(glyph)
	if header != "" {
		line += " " + header
	}
	if trailer != "" {
		line += " " + styles.MutedStyle.Render(trailer)
	}

	// Keep subagent transcript cards brief. Lifecycle cards read like compact
	// status rows, mirroring the delegation tool-call styling. Only failures
	// earn a second line because the error text is actionable.
	body := line
	if info.Kind == types.SubAgentEventFailed {
		detail := strings.TrimSpace(info.Detail)
		if info.Truncated {
			detail = strings.TrimRight(detail, " …") + "…"
		}
		if detail != "" {
			body = line + "\n" + styles.MutedStyle.Render(detail)
		}
	}

	// Wrap with ToolMessageStyle (no padding) instead of
	// AssistantMessageStyle (which has Padding(0, 1)) so a lifecycle card and
	// its surrounding tool rows share the exact same horizontal alignment
	// and tight vertical rhythm. Selection is intentionally not styled here
	// because the surrounding tool rows are not styled on selection either;
	// keeping that consistent across rows avoids one row visually "jumping"
	// out of the cluster as the user navigates with arrow keys.
	return styles.ToolMessageStyle.Width(width).Render(body)
}

// SubAgentShortRef returns the short subagent reference represented by this
// message view, if any. Tool rows and runtime-driven lifecycle cards both use
// it so the transcript can open/attach to the live subagent session when the
// user clicks the rendered `[agent] (id)` token.
func (mv *messageModel) SubAgentShortRef() string {
	if mv == nil || mv.message == nil {
		return ""
	}
	if mv.message.Type != types.MessageTypeSubAgent || mv.message.SubAgent == nil {
		return ""
	}
	return strings.TrimSpace(mv.message.SubAgent.ShortID)
}

// Height calculates the height needed for this message view
func (mv *messageModel) Height(width int) int {
	content := mv.Render(width)
	return strings.Count(content, "\n") + 1
}

// Message returns the underlying message
func (mv *messageModel) Message() *types.Message {
	return mv.message
}

// Layout.Sizeable methods

// StopAnimation stops the spinner animation and unregisters from the animation coordinator.
// This must be called when the view is removed from the UI to avoid leaked animation subscriptions.
func (mv *messageModel) StopAnimation() {
	if mv.message.Type == types.MessageTypeSpinner || mv.message.Type == types.MessageTypeLoading {
		mv.spinner.Stop()
	}
}

// SetSize sets the dimensions of the message view
func (mv *messageModel) SetSize(width, height int) tea.Cmd {
	mv.width = width
	mv.height = height
	return nil
}

// GetSize returns the current dimensions
func (mv *messageModel) GetSize() (width, height int) {
	return mv.width, mv.height
}

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// preserveLineBreaks preserves leading indentation by converting leading spaces
// to non-breaking spaces (U+00A0) which won't be stripped by markdown parsers.
// Line breaks are handled by glamour.WithPreservedNewLines().
func preserveLineBreaks(s string) string {
	if !strings.Contains(s, "\n") {
		return preserveIndentation(s)
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = preserveIndentation(line)
	}
	return strings.Join(lines, "\n")
}

// preserveIndentation converts leading spaces in a line to non-breaking spaces (U+00A0).
// This prevents markdown parsers from stripping leading whitespace while maintaining
// the same visual appearance in terminal output.
func preserveIndentation(line string) string {
	if line == "" || line[0] != ' ' {
		return line
	}
	leadingSpaces := 0
	for _, c := range line {
		if c == ' ' {
			leadingSpaces++
		} else {
			break
		}
	}
	if leadingSpaces == 0 {
		return line
	}
	return strings.Repeat("\u00A0", leadingSpaces) + line[leadingSpaces:]
}
