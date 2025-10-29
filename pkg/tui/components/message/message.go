package message

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/v2/spinner"
	tea "github.com/charmbracelet/bubbletea/v2"

	"github.com/docker/cagent/pkg/tui/components/markdown"
	"github.com/docker/cagent/pkg/tui/core/layout"
	"github.com/docker/cagent/pkg/tui/styles"
	"github.com/docker/cagent/pkg/tui/types"
)

// Model represents a view that can render a message
type Model interface {
	layout.Model
	layout.Sizeable
	SetMessage(msg *types.Message)
}

// messageModel implements Model
type messageModel struct {
	message *types.Message
	width   int
	height  int
	focused bool
	spinner spinner.Model

	// Rendering cache
	cachedView  string
	cachedWidth int
}

// New creates a new message view
func New(msg *types.Message) *messageModel {
	return &messageModel{
		message: msg,
		width:   80, // Default width
		height:  1,  // Will be calculated
		focused: false,
		spinner: spinner.New(spinner.WithSpinner(spinner.Points)),
	}
}

// Bubble Tea Model methods

// Init initializes the message view
func (mv *messageModel) Init() tea.Cmd {
	if mv.message.Type == types.MessageTypeSpinner {
		return mv.spinner.Tick
	}
	return nil
}

func (mv *messageModel) SetMessage(msg *types.Message) {
	mv.message = msg
	// Invalidate cache when message changes
	mv.cachedView = ""
	mv.cachedWidth = 0
}

// Update handles messages and updates the message view state
func (mv *messageModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if mv.message.Type == types.MessageTypeSpinner {
		var cmd tea.Cmd
		mv.spinner, cmd = mv.spinner.Update(msg)
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

	// Check if we can use cached rendering
	// Only cache static content that won't change between renders
	// Allow caching for types that don't require content (separator, cancelled)
	canCache := msg.Type != types.MessageTypeSpinner &&
		(msg.Content != "" || msg.Type == types.MessageTypeSeparator || msg.Type == types.MessageTypeCancelled)

	if canCache && mv.cachedView != "" && mv.cachedWidth == width {
		return mv.cachedView
	}

	var rendered string

	switch msg.Type {
	case types.MessageTypeSpinner:
		return mv.spinner.View()
	case types.MessageTypeUser:
		if r, err := markdown.NewRenderer(width - len(styles.UserMessageBorderStyle.Render(""))).Render(msg.Content); err == nil {
			rendered = styles.UserMessageBorderStyle.Render(strings.TrimRight(r, "\n\r\t "))
		} else {
			rendered = msg.Content
		}

	case types.MessageTypeAssistant:
		if msg.Content == "" {
			return mv.spinner.View()
		}

		text := senderPrefix(msg.Sender) + msg.Content
		r, err := markdown.NewRenderer(width).Render(text)
		if err != nil {
			rendered = text
		} else {
			rendered = strings.TrimRight(r, "\n\r\t ")
		}

	case types.MessageTypeAssistantReasoning:
		if msg.Content == "" {
			return mv.spinner.View()
		}
		text := "Thinking: " + senderPrefix(msg.Sender) + msg.Content
		// Render through the markdown renderer to ensure proper wrapping to width
		r, err := markdown.NewRenderer(width).Render(text)
		if err != nil {
			rendered = styles.MutedStyle.Italic(true).Render(text)
		} else {
			// Strip ANSI from inner rendering so muted style fully applies
			clean := stripANSI(strings.TrimRight(r, "\n\r\t "))
			rendered = styles.MutedStyle.Italic(true).Render(clean)
		}

	case types.MessageTypeShellOutput:
		if r, err := markdown.NewRenderer(width).Render(fmt.Sprintf("```console\n%s\n```", msg.Content)); err == nil {
			rendered = strings.TrimRight(r, "\n\r\t ")
		} else {
			rendered = msg.Content
		}
	case types.MessageTypeSeparator:
		rendered = styles.MutedStyle.Render("•" + strings.Repeat("─", mv.width-3) + "•")
	case types.MessageTypeCancelled:
		rendered = styles.WarningStyle.Render("⚠ stream cancelled ⚠")
	case types.MessageTypeError:
		rendered = styles.ErrorStyle.Render("│ " + msg.Content)
	case types.MessageTypeWarning:
		rendered = styles.WarningStyle.Render(msg.Content)
	case types.MessageTypeSystem:
		rendered = styles.MutedStyle.Render("ℹ " + msg.Content)
	default:
		rendered = msg.Content
	}

	if canCache {
		mv.cachedView = rendered
		mv.cachedWidth = width
	}

	return rendered
}

func senderPrefix(sender string) string {
	if sender == "" || sender == "root" {
		return ""
	}
	return fmt.Sprintf("%s: ", sender)
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

// SetSize sets the dimensions of the message view
func (mv *messageModel) SetSize(width, height int) tea.Cmd {
	// Invalidate cache if width changes (height doesn't affect rendering)
	if mv.width != width {
		mv.cachedView = ""
		mv.cachedWidth = 0
	}
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
