package sidebar

import (
	"fmt"
	"os"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/tools"
	"github.com/docker/cagent/pkg/tui/components/spinner"
	"github.com/docker/cagent/pkg/tui/components/tool/todotool"
	"github.com/docker/cagent/pkg/tui/core/layout"
	"github.com/docker/cagent/pkg/tui/service"
	"github.com/docker/cagent/pkg/tui/styles"
)

type Mode int

const (
	ModeVertical Mode = iota
	ModeHorizontal
)

type Model interface {
	layout.Model
	layout.Sizeable

	SetTokenUsage(event *runtime.TokenUsageEvent)
	SetTodos(toolCall tools.ToolCall) error
	SetWorking(working bool) tea.Cmd
	SetMode(mode Mode)
	GetSize() (width, height int)
}

type model struct {
	width        int
	height       int
	sessionUsage map[string]*runtime.Usage // sessionID -> latest usage (replaces, not accumulates)
	sessionAgent map[string]string         // sessionID -> agentName
	todoComp     *todotool.SidebarComponent
	working      bool
	mcpInit      bool
	spinner      spinner.Spinner
	mode         Mode
	sessionTitle string
}

func New(manager *service.TodoManager) Model {
	return &model{
		width:        20,
		height:       24,
		sessionUsage: make(map[string]*runtime.Usage),
		sessionAgent: make(map[string]string),
		todoComp:     todotool.NewSidebarComponent(manager),
		spinner:      spinner.New(spinner.ModeSpinnerOnly),
		sessionTitle: "New session",
	}
}

func (m *model) Init() tea.Cmd {
	return nil
}

func (m *model) SetTokenUsage(event *runtime.TokenUsageEvent) {
	if event == nil || event.Usage == nil || event.SessionID == "" || event.AgentContext.AgentName == "" {
		return
	}

	// Store/replace by session ID (each event has cumulative totals for that session)
	usage := *event.Usage
	m.sessionUsage[event.SessionID] = &usage
	m.sessionAgent[event.SessionID] = event.AgentContext.AgentName
}

func (m *model) SetTodos(toolCall tools.ToolCall) error {
	return m.todoComp.SetTodos(toolCall)
}

// SetWorking sets the working state and returns a command to start the spinner if needed
func (m *model) SetWorking(working bool) tea.Cmd {
	m.working = working
	if working {
		return m.spinner.Init()
	}
	return nil
}

// formatTokenCount formats a token count with K/M suffixes for readability
func formatTokenCount(count int) string {
	if count >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(count)/1000000)
	} else if count >= 1000 {
		return fmt.Sprintf("%.1fK", float64(count)/1000)
	}
	return fmt.Sprintf("%d", count)
}

// getCurrentWorkingDirectory returns the current working directory with home directory replaced by ~/
func getCurrentWorkingDirectory() string {
	pwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Replace home directory with ~/
	if homeDir, err := os.UserHomeDir(); err == nil && strings.HasPrefix(pwd, homeDir) {
		pwd = "~" + pwd[len(homeDir):]
	}

	return pwd
}

// Update handles messages and updates the component state
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := m.SetSize(msg.Width, msg.Height)
		return m, cmd
	case *runtime.MCPInitStartedEvent:
		m.mcpInit = true
		return m, m.spinner.Init()
	case *runtime.MCPInitFinishedEvent:
		m.mcpInit = false
		return m, nil
	case *runtime.SessionTitleEvent:
		m.sessionTitle = msg.Title
		return m, nil
	default:
		if m.working || m.mcpInit {
			var cmd tea.Cmd
			var model layout.Model
			model, cmd = m.spinner.Update(msg)
			m.spinner = model.(spinner.Spinner)
			return m, cmd
		}
		return m, nil
	}
}

// View renders the component
func (m *model) View() string {
	if m.mode == ModeVertical {
		return m.verticalView()
	}

	return m.horizontalView()
}

func (m *model) horizontalView() string {
	pwd := getCurrentWorkingDirectory()
	usageSummary := m.tokenUsageSummary()
	gapWidth := m.width - lipgloss.Width(pwd) - lipgloss.Width(usageSummary) - 2
	title := m.sessionTitle + " " + m.workingIndicator()
	return lipgloss.JoinVertical(lipgloss.Top, title, fmt.Sprintf("%s%*s%s", styles.MutedStyle.Render(pwd), gapWidth, "", usageSummary))
}

func (m *model) verticalView() string {
	topContent := m.sessionTitle + "\n"

	if pwd := getCurrentWorkingDirectory(); pwd != "" {
		topContent += styles.MutedStyle.Render(pwd) + "\n\n"
	}

	// Show working indicator before usage so it doesn't jump around
	if indicator := m.workingIndicator(); indicator != "" {
		topContent += indicator + "\n\n"
	}

	topContent += m.tokenUsageDetails()

	m.todoComp.SetSize(m.width)
	todoContent := strings.TrimSuffix(m.todoComp.Render(), "\n")

	// Calculate available height for content
	availableHeight := m.height - 2 // Account for borders
	topHeight := strings.Count(topContent, "\n") + 1
	todoHeight := strings.Count(todoContent, "\n") + 1

	// Calculate padding needed to push todos to bottom
	paddingHeight := max(availableHeight-topHeight-todoHeight, 0)
	for range paddingHeight {
		topContent += "\n"
	}
	topContent += todoContent

	return styles.BaseStyle.
		Width(m.width).
		Height(m.height-2).
		Align(lipgloss.Left, lipgloss.Top).
		Render(topContent)
}

func (m *model) workingIndicator() string {
	if m.mcpInit || m.working {
		label := "Working..."
		if m.mcpInit {
			label = "Initializing MCP servers..."
		}
		indicator := styles.ActiveStyle.Render(m.spinner.View() + " " + label)
		return indicator
	}

	return ""
}

func (m *model) tokenUsageSummary() string {
	if len(m.sessionUsage) == 0 {
		return ""
	}

	var totalTokens int
	var totalCost float64
	var contextLimit int
	var contextLength int

	for _, usage := range m.sessionUsage {
		totalTokens += usage.InputTokens + usage.OutputTokens
		totalCost += usage.Cost
		if usage.ContextLimit > contextLimit {
			contextLimit = usage.ContextLimit
		}
		contextLength += usage.ContextLength
	}

	parts := []string{
		fmt.Sprintf("%s tokens", formatTokenCount(totalTokens)),
		fmt.Sprintf("$%.2f", totalCost),
	}

	// Only show context % if no delegation has occurred (single session only)
	if len(m.sessionUsage) == 1 && contextLimit > 0 {
		percent := (float64(contextLength) / float64(contextLimit)) * 100
		parts = append(parts, fmt.Sprintf("Context: %.0f%%", percent))
	}

	return styles.SubtleStyle.Render(strings.Join(parts, " | "))
}

func (m *model) tokenUsageDetails() string {
	if len(m.sessionUsage) == 0 {
		return ""
	}

	// Aggregate by agent name from session data
	agentUsage := make(map[string]*runtime.Usage)
	for sessionID, usage := range m.sessionUsage {
		agentName := m.sessionAgent[sessionID]
		if agentName == "" {
			continue
		}

		if existing, ok := agentUsage[agentName]; ok {
			// Accumulate across different sessions
			existing.InputTokens += usage.InputTokens
			existing.OutputTokens += usage.OutputTokens
			existing.Cost += usage.Cost
		} else {
			// First session for this agent
			u := *usage
			agentUsage[agentName] = &u
		}
	}

	var builder strings.Builder
	var totalTokens int
	var totalCost float64
	var contextLimit int
	var contextLength int

	for _, usage := range m.sessionUsage {
		totalTokens += usage.InputTokens + usage.OutputTokens
		totalCost += usage.Cost
		if usage.ContextLimit > contextLimit {
			contextLimit = usage.ContextLimit
		}
		contextLength += usage.ContextLength
	}

	builder.WriteString(styles.SubtleStyle.Render("Total Usage"))
	builder.WriteString(fmt.Sprintf("\n  Tokens: %s | Cost: $%.2f", formatTokenCount(totalTokens), totalCost))

	// Only show context % if no delegation has occurred (single session only)
	if len(m.sessionUsage) == 1 && contextLimit > 0 {
		percent := (float64(contextLength) / float64(contextLimit)) * 100
		builder.WriteString(fmt.Sprintf(" | %.0f%%", percent))
	}

	// Show breakdown only when multiple agents have reported
	if len(agentUsage) > 1 {
		builder.WriteString("\n--------------------------------\n")
		builder.WriteString(styles.SubtleStyle.Render("Agent Breakdown"))

		names := make([]string, 0, len(agentUsage))
		for name := range agentUsage {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			usage := agentUsage[name]
			tokens := usage.InputTokens + usage.OutputTokens
			builder.WriteString(fmt.Sprintf("\n  %s: %s tokens | $%.2f", name, formatTokenCount(tokens), usage.Cost))
		}
	}

	return builder.String()
}

// SetSize sets the dimensions of the component
func (m *model) SetSize(width, height int) tea.Cmd {
	m.width = width
	m.height = height
	m.todoComp.SetSize(width)
	return nil
}

// GetSize returns the current dimensions
func (m *model) GetSize() (width, height int) {
	return m.width, m.height
}

func (m *model) SetMode(mode Mode) {
	m.mode = mode
}
