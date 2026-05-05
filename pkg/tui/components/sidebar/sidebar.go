package sidebar

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Mode int

const (
	ModeVertical Mode = iota
	ModeCollapsed
)

// Model represents a sidebar component
type Model interface {
	layout.Model
	layout.Sizeable
	layout.Positionable

	SetTokenUsage(event *runtime.TokenUsageEvent)
	SetTodos(result *tools.ToolCallResult) error
	SetMode(mode Mode)
	SetAgentInfo(agentName, model, description string) tea.Cmd
	SetTeamInfo(availableAgents []runtime.AgentDetails)
	SetAgentSwitching(switching bool)
	SetToolsetInfo(availableTools int, loading bool)
	SetSkillsInfo(availableSkills int)
	SetSessionStarred(starred bool)
	SetQueuedMessages(messages ...string)
	SeedSubagentsFromLiveTree(nodes []runtime.LiveSessionNode)
	ClearTransientHover()
	GetSize() (width, height int)
	LoadFromSession(sess *session.Session)
	// HandleClick checks if click is on the star or title and returns true if handled
	HandleClick(x, y int) bool
	// HandleClickType returns the type of click (star, title, agent, or none).
	// For ClickAgent, the second return value is the agent name.
	HandleClickType(x, y int) (ClickResult, string)
	// IsCollapsed returns whether the sidebar is collapsed
	IsCollapsed() bool
	// ToggleCollapsed toggles the collapsed state
	ToggleCollapsed()
	// SetCollapsed sets the collapsed state directly
	SetCollapsed(collapsed bool)
	// CollapsedHeight returns the number of lines needed for collapsed mode
	CollapsedHeight(contentWidth int) int
	// GetPreferredWidth returns the user's preferred width (for resize persistence)
	GetPreferredWidth() int
	// SetPreferredWidth sets the user's preferred width
	SetPreferredWidth(width int)
	// ClampWidth ensures width is within valid bounds for the given window width
	ClampWidth(width, windowInnerWidth int) int
	// HandleTitleClick handles a click on the title area and returns true if
	// edit mode should start (on double-click)
	HandleTitleClick() bool
	// BeginTitleEdit starts inline editing of the session title
	BeginTitleEdit()
	// IsEditingTitle returns true if the title is being edited
	IsEditingTitle() bool
	// CommitTitleEdit commits the current title edit and returns the new title
	CommitTitleEdit() string
	// CancelTitleEdit cancels the current title edit
	CancelTitleEdit()
	// UpdateTitleInput passes a key message to the title input
	UpdateTitleInput(msg tea.Msg) tea.Cmd
	// SetTitleRegenerating sets the title regeneration state and returns a command to start/stop spinner
	SetTitleRegenerating(regenerating bool) tea.Cmd
	// IsScrollbarDragging returns true when the scrollbar thumb is being dragged.
	IsScrollbarDragging() bool
	// WorkingDirectory returns the working directory path displayed in the sidebar.
	WorkingDirectory() string
}

// ragIndexingState tracks per-strategy indexing progress
type ragIndexingState struct {
	current int
	total   int
	spinner spinner.Spinner
}

// model implements Model
type model struct {
	width             int
	height            int
	xPos              int                       // absolute x position on screen
	yPos              int                       // absolute y position on screen
	layoutCfg         LayoutConfig              // layout configuration for spacing
	sessionUsage      map[string]*runtime.Usage // sessionID -> latest usage snapshot
	sessionAgent      map[string]string         // sessionID -> agent name
	todoComp          *todotool.SidebarComponent
	mcpInit           bool
	ragIndexing       map[string]*ragIndexingState // strategy name -> indexing state
	spinner           spinner.Spinner
	spinnerActive     bool // true when spinner is registered with animation coordinator
	mode              Mode
	sessionTitle      string
	sessionStarred    bool
	sessionHasContent bool // true when session has been used (has messages)
	currentAgent      string
	agentModel        string
	agentDescription  string
	availableAgents   []runtime.AgentDetails
	agentSwitching    bool
	availableTools    int
	availableSkills   int
	toolsLoading      bool // true when more tools may still be loading
	sessionState      *service.SessionState
	workingAgent      string // Name of the agent currently working (empty if none)
	currentSessionID  string // Session ID of the currently active stream
	scrollview        *scrollview.Model
	workingDirectory  string
	queuedMessages    []string // Truncated preview of queued messages
	subagents         map[string]*subagentState
	streamCancelled   bool // true after ESC cancel until next TurnStartedEvent
	collapsed         bool // true when sidebar is collapsed
	// parentIdle is true while the parent turn is suspended waiting on
	// subagents. In this state the sidebar must not spin the parent agent
	// row — the global working spinner is also off.
	parentIdle         bool
	titleRegenerating  bool // true when title is being regenerated by AI
	titleGenerated     bool // true once a title has been generated or set (hides pencil until then)
	preferredWidth     int  // user's preferred width (persisted across collapse/expand)
	editingTitle       bool // true when inline title editing is active
	titleInput         textinput.Model
	lastTitleClickTime time.Time // for double-click detection on title

	// Render cache to avoid re-rendering sections on every frame during scroll
	cachedLines          []string // Cached rendered lines
	cachedWidth          int      // Width used for cached render
	cachedNeedsScrollbar bool     // Whether scrollbar is needed for cached render
	cacheDirty           bool     // True when cache needs rebuild

	// Agent click zones: maps content line index to agent name for click detection
	agentClickZones map[int]string // content line -> agent name

	// Subagent click zones: maps content line index to subagent full session id.
	// Callers that need the short ref derive it via subagent.ShortRef.
	subagentClickZones map[int]string // content line -> subagent full id

	// hoveredParentLine is true while the mouse is over the clickable
	// `parent: ...` line in the Session tab of an attached child-session.
	// It is used purely for rendering the subtle `↩ parent` affordance on
	// hover; the line remains clickable even when the affordance is hidden.
	hoveredParentLine bool

	// hoveredSubagentID is the full id of the subagent whose row the mouse
	// is currently over. Empty when no subagent row is hovered. Used to swap
	// the right-aligned status chip for a relative "x ago" badge on hover.
	hoveredSubagentID string
}

// Option is a functional option for configuring the sidebar.
type Option func(*model)

// WithLayoutConfig sets a custom layout configuration.
func WithLayoutConfig(cfg LayoutConfig) Option {
	return func(m *model) { m.layoutCfg = cfg }
}

func New(sessionState *service.SessionState, opts ...Option) Model {
	ti := textinput.New()
	ti.Placeholder = "Session title"
	ti.CharLimit = 50
	ti.Prompt = "" // No prompt to maximize usable width in collapsed sidebar

	m := &model{
		width:        20,
		layoutCfg:    DefaultLayoutConfig(),
		height:       24,
		sessionUsage: make(map[string]*runtime.Usage),
		sessionAgent: make(map[string]string),
		todoComp:     todotool.NewSidebarComponent(),
		subagents:    make(map[string]*subagentState),
		spinner:      spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle),
		sessionTitle: "New session",
		ragIndexing:  make(map[string]*ragIndexingState),
		sessionState: sessionState,
		scrollview: scrollview.New(
			scrollview.WithWheelStep(1),
			scrollview.WithKeyMap(nil), // Sidebar has no keyboard scroll — only mouse
		),
		workingDirectory: getCurrentWorkingDirectory(),
		preferredWidth:   DefaultWidth,
		titleInput:       ti,
		cacheDirty:       true, // Initial render needed
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

func (m *model) Init() tea.Cmd {
	return nil
}

// needsSpinner returns true if any spinner-driving state is active.
func (m *model) needsSpinner() bool {
	// Keep the shared sidebar spinner ticking whenever it has to render a live
	// frame. Live background subagents matter here even when the parent has
	// gone idle: their rows rely on m.spinner.RawFrame() to animate.
	return m.workingAgent != "" || m.toolsLoading || m.mcpInit || m.titleRegenerating || m.liveSubagentCount() > 0
}

// startSpinner registers the spinner with the animation coordinator if not already active.
// Safe to call multiple times - only the first call registers.
func (m *model) startSpinner() tea.Cmd {
	if m.spinnerActive {
		return nil // Already registered
	}
	m.spinnerActive = true
	return m.spinner.Init()
}

// stopSpinner unregisters the spinner from the animation coordinator if no state needs it.
// Only actually stops if currently active AND no spinner-driving state remains.
func (m *model) stopSpinner() {
	if !m.spinnerActive {
		return // Not registered
	}
	if m.needsSpinner() {
		return // Still needed by another state
	}
	m.spinnerActive = false
	m.spinner.Stop()
}

// invalidateCache marks the sidebar render cache as dirty so it will be rebuilt on the next View().
func (m *model) invalidateCache() {
	m.cacheDirty = true
}

func (m *model) SetTokenUsage(event *runtime.TokenUsageEvent) {
	if event == nil || event.Usage == nil || event.SessionID == "" || event.AgentName == "" {
		return
	}

	// Store/replace by session ID (each event has cumulative totals for that session)
	usage := *event.Usage
	m.sessionUsage[event.SessionID] = &usage
	m.sessionAgent[event.SessionID] = event.AgentName

	// Mark session as having content once we receive token usage
	m.sessionHasContent = true
	m.invalidateCache()
}

func (m *model) SetTodos(result *tools.ToolCallResult) error {
	m.invalidateCache()
	return m.todoComp.SetTodos(result)
}

// SetAgentInfo sets the current agent information and updates the model in availableAgents.
// It no-ops when the values are unchanged to avoid unnecessary cache invalidation and re-renders.
func (m *model) SetAgentInfo(agentName, modelID, description string) tea.Cmd {
	if m.currentAgent == agentName && m.agentModel == modelID && m.agentDescription == description {
		return nil
	}

	m.currentAgent = agentName
	m.agentModel = modelID
	m.agentDescription = description

	// Update the provider and model in availableAgents for the current agent.
	// This is important when fallback models from different providers are used.
	// Parse "provider/model" format using first slash to handle model names containing slashes
	// (e.g., "dmr/ai/llama3.2" → Provider="dmr", Model="ai/llama3.2").
	for i := range m.availableAgents {
		if m.availableAgents[i].Name == agentName && modelID != "" {
			if provider, modelName, found := strings.Cut(modelID, "/"); found {
				m.availableAgents[i].Provider = provider
				m.availableAgents[i].Model = modelName
			} else {
				// No slash in modelID; treat the whole string as model name
				m.availableAgents[i].Model = modelID
			}
			break
		}
	}
	m.invalidateCache()
	return nil
}

// SetTeamInfo sets the available agents in the team
func (m *model) SetTeamInfo(availableAgents []runtime.AgentDetails) {
	m.availableAgents = availableAgents
	m.invalidateCache()
}

// SetAgentSwitching sets whether an agent switch is in progress
func (m *model) SetAgentSwitching(switching bool) {
	m.agentSwitching = switching
	m.invalidateCache()
}

// SetToolsetInfo sets the number of available tools and loading state
func (m *model) SetToolsetInfo(availableTools int, loading bool) {
	m.availableTools = availableTools
	m.toolsLoading = loading
	m.invalidateCache()
}

// SetSkillsInfo sets the number of available skills
func (m *model) SetSkillsInfo(availableSkills int) {
	m.availableSkills = availableSkills
	m.invalidateCache()
}

// SetSessionStarred sets the starred status of the current session
func (m *model) SetSessionStarred(starred bool) {
	m.sessionStarred = starred
	m.invalidateCache()
}

// SetQueuedMessages sets the list of queued message previews to display
func (m *model) SetQueuedMessages(queuedMessages ...string) {
	m.queuedMessages = queuedMessages
	m.invalidateCache()
}

// SetTitleRegenerating sets the title regeneration state and manages spinner lifecycle.
// Returns a command to start the spinner if regenerating, nil otherwise.
func (m *model) SetTitleRegenerating(regenerating bool) tea.Cmd {
	m.titleRegenerating = regenerating
	m.invalidateCache()
	if regenerating {
		return m.startSpinner()
	}
	m.stopSpinner()
	return nil
}

func (m *model) IsScrollbarDragging() bool {
	return m.scrollview.IsDragging()
}

// WorkingDirectory returns the working directory path displayed in the sidebar.
func (m *model) WorkingDirectory() string {
	return m.workingDirectory
}

// ClickResult indicates what was clicked in the sidebar
type ClickResult int

const (
	ClickNone ClickResult = iota
	ClickStar
	ClickTitle      // Click on the title area (use double-click to edit)
	ClickWorkingDir // Click on the working directory line
	ClickParent     // Click on the parent session line in attached sub-session tabs
	ClickAgent      // Click on an agent name in the sidebar
	ClickSubagent   // Click on a subagent row in the Subagents section
)

// HandleClick checks if click is on the star or title and returns true if it was
// x and y are coordinates relative to the sidebar's top-left corner
// This does NOT toggle the state - caller should handle that
func (m *model) HandleClick(x, y int) bool {
	result, _ := m.HandleClickType(x, y)
	return result != ClickNone
}

// HandleClickType returns what was clicked (star, title, working dir, agent, or nothing).
// For ClickAgent, the second return value is the agent name.
func (m *model) HandleClickType(x, y int) (ClickResult, string) {
	// Account for left padding
	adjustedX := x - m.layoutCfg.PaddingLeft
	if adjustedX < 0 {
		return ClickNone, ""
	}

	if m.mode == ModeCollapsed {
		// In collapsed mode, title starts at line 0.
		titleStartY := 0
		titleLines := m.titleLineCount()

		// Check if click is within the title area (line titleStartY to titleStartY+titleLines-1)
		if y >= titleStartY && y < titleStartY+titleLines {
			// Check if click is on the star (first line only, first few chars)
			if y == titleStartY && m.sessionHasContent && adjustedX <= starClickWidth {
				return ClickStar, ""
			}
			// Click is on title area (for double-click to edit)
			if m.titleGenerated && !m.editingTitle {
				return ClickTitle, ""
			}
		}

		vm := m.computeCollapsedViewModel(m.contentWidth(false))
		sessionInfoLines := vm.titleSectionLines()

		// In collapsed mode the working-directory block comes immediately after
		// the title section. The optional parent line lives below the working
		// directory so the Session section's ordering matches vertical mode.
		wdStartY := sessionInfoLines
		wdLines := 0
		if m.workingDirectory != "" {
			wdLines = linesNeeded(lipgloss.Width(vm.WorkingDir), vm.ContentWidth)
			if y >= wdStartY && y < wdStartY+wdLines {
				return ClickWorkingDir, ""
			}
		}

		if vm.ParentLine != "" {
			parentY := wdStartY
			if wdLines > 0 {
				parentY += wdLines + 1 // one blank spacer above parent line
			}
			if y == parentY {
				return ClickParent, m.sessionState.ParentSessionID()
			}
		}

		return ClickNone, ""
	}

	// In vertical mode, the title starts at verticalStarY.
	scrollOffset := m.scrollview.ScrollOffset()
	contentY := y + scrollOffset // Convert viewport Y to content Y
	titleY := verticalStarY
	titleLines := m.titleLineCount()

	// Check if click is within the title area
	if contentY >= titleY && contentY < titleY+titleLines {
		// Check if click is on the star (first line only, first few chars)
		if contentY == titleY && m.sessionHasContent && adjustedX <= starClickWidth {
			return ClickStar, ""
		}
		// Click is on title area (for double-click to edit)
		if m.titleGenerated && !m.editingTitle {
			return ClickTitle, ""
		}
	}

	nextY := titleY + titleLines + 1 // blank separator line after title block

	// Working dir sits first after the title separator.
	workingDirY := nextY
	if m.workingDirectory != "" && contentY == workingDirY {
		return ClickWorkingDir, ""
	}

	// Parent line, when present, is rendered below the working directory with
	// one blank spacer row above it.
	if m.sessionState != nil && m.sessionState.IsSubSession() && m.sessionState.ParentSessionID() != "" {
		parentY := nextY
		if m.workingDirectory != "" {
			parentY = workingDirY + 2
		}
		if contentY == parentY {
			return ClickParent, m.sessionState.ParentSessionID()
		}
	}

	// Check if click is on an agent name
	if agentName, ok := m.agentClickZones[contentY]; ok {
		return ClickAgent, agentName
	}

	// Check if click is on a subagent row. The stored value is the full
	// session id so callers can attach a live tab; ShortRef can still be
	// derived on the fly when only the short form is needed.
	if fullID, ok := m.subagentClickZones[contentY]; ok {
		return ClickSubagent, fullID
	}

	return ClickNone, ""
}

// titleLineCount returns the number of lines the title occupies when rendered.
func (m *model) titleLineCount() int {
	if !m.titleGenerated || m.sessionTitle == "" {
		return 1
	}
	contentWidth := m.contentWidth(false)
	if contentWidth <= 0 {
		return 1
	}
	// Calculate width: star + title
	starWidth := lipgloss.Width(m.starIndicator())
	titleWidth := lipgloss.Width(m.sessionTitle)
	totalWidth := starWidth + titleWidth
	return max(1, (totalWidth+contentWidth-1)/contentWidth)
}

// updateSubagentHoverAt updates hoveredSubagentID using absolute screen
// coordinates. Returns true when the hovered row changed.
func (m *model) updateSubagentHoverAt(x, y int) bool {
	changed := m.updateParentLineHoverAt(x, y)
	if m.mode != ModeVertical || len(m.subagentClickZones) == 0 {
		if m.setHoveredSubagent("") {
			changed = true
		}
		return changed
	}

	contentX := x - (m.xPos + m.layoutCfg.PaddingLeft)
	if contentX < 0 || contentX >= m.contentWidth(m.cachedNeedsScrollbar) {
		if m.setHoveredSubagent("") {
			changed = true
		}
		return changed
	}

	contentY := y + m.scrollview.ScrollOffset()
	if fullID, ok := m.subagentClickZones[contentY]; ok {
		if m.setHoveredSubagent(fullID) {
			changed = true
		}
		return changed
	}
	if m.setHoveredSubagent("") {
		changed = true
	}
	return changed
}

// updateParentLineHoverAt updates hoveredParentLine for the current mouse
// position. Returns true when the hover state actually changed.
func (m *model) updateParentLineHoverAt(x, y int) bool {
	if m.sessionState == nil || !m.sessionState.IsSubSession() || m.sessionState.ParentSessionID() == "" {
		if m.hoveredParentLine {
			m.hoveredParentLine = false
			return true
		}
		return false
	}

	adjustedX := x - (m.xPos + m.layoutCfg.PaddingLeft)
	if adjustedX < 0 || adjustedX >= m.contentWidth(m.cachedNeedsScrollbar) {
		if m.hoveredParentLine {
			m.hoveredParentLine = false
			return true
		}
		return false
	}

	contentY := y
	if m.mode == ModeVertical {
		contentY += m.scrollview.ScrollOffset()
	}

	isParentLine := false
	if m.mode == ModeCollapsed {
		vm := m.computeCollapsedViewModel(m.contentWidth(false))
		if vm.ParentLine != "" {
			parentY := vm.titleSectionLines()
			if m.workingDirectory != "" {
				wdLines := linesNeeded(lipgloss.Width(vm.WorkingDir), vm.ContentWidth)
				parentY += wdLines + 1
			}
			isParentLine = contentY == parentY
		}
	} else {
		titleLines := m.titleLineCount()
		parentY := verticalStarY + titleLines + 1
		if m.workingDirectory != "" {
			// Parent line sits below the working directory with a blank spacer.
			parentY += 2
		}
		isParentLine = contentY == parentY
	}
	if m.hoveredParentLine == isParentLine {
		return false
	}
	m.hoveredParentLine = isParentLine
	return true
}

// ClearTransientHover clears purely mouse-driven hover affordances that should
// never survive a tab switch. This is deliberately narrower than a full reset:
// it does not touch durable sidebar data (agents, usage, subagents, etc.),
// only ephemeral visual state like the hovered parent row / hovered subagent.
func (m *model) ClearTransientHover() {
	changed := false
	if m.hoveredParentLine {
		m.hoveredParentLine = false
		changed = true
	}
	if m.hoveredSubagentID != "" {
		m.hoveredSubagentID = ""
		changed = true
	}
	if changed {
		m.invalidateCache()
	}
}

// LoadFromSession loads sidebar state from a restored session
func (m *model) LoadFromSession(sess *session.Session) {
	if sess == nil {
		return
	}

	// Use TotalCost to include sub-session costs (handles older sessions
	// where the parent's Cost field did not include sub-session costs).
	totalCost := sess.TotalCost()

	// Load token usage from session
	if sess.InputTokens > 0 || sess.OutputTokens > 0 || totalCost > 0 {
		m.sessionUsage[sess.ID] = &runtime.Usage{
			InputTokens:   sess.InputTokens,
			OutputTokens:  sess.OutputTokens,
			ContextLength: sess.InputTokens + sess.OutputTokens,
			Cost:          totalCost,
		}
	}

	// Load session title
	if title := sess.GetTitle(); title != "" {
		m.sessionTitle = title
		m.titleGenerated = true // Mark as generated since session already has a title
	}

	// Load starred status
	m.sessionStarred = sess.Starred

	// Load working directory from session
	if sess.WorkingDir != "" {
		wd := sess.WorkingDir
		if homeDir := paths.GetHomeDir(); homeDir != "" && strings.HasPrefix(wd, homeDir) {
			wd = "~" + wd[len(homeDir):]
		}
		m.workingDirectory = wd
	}

	// Session has content if it has messages or token usage
	m.sessionHasContent = len(sess.Messages) > 0 || sess.InputTokens > 0 || sess.OutputTokens > 0

	m.invalidateCache()
}

// formatTokenCount formats a token count with K/M suffixes for readability
func formatTokenCount(count int64) string {
	if count >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(count)/1000000)
	} else if count >= 1000 {
		return fmt.Sprintf("%.1fK", float64(count)/1000)
	}
	return strconv.FormatInt(count, 10)
}

func formatCost(cost float64) string {
	return fmt.Sprintf("%.2f", cost)
}

// currentSessionUsage returns the usage snapshot for the current agent's session.
// It uses a 3-tier lookup: session ID → agent name → single-session fallback.
func (m *model) currentSessionUsage() (*runtime.Usage, bool) {
	// Direct lookup by current session ID, skipping when the session belongs
	// to a different agent (stale after a sub-agent's stream stops while
	// currentAgent has already been restored to the parent).
	if m.currentSessionID != "" {
		owner := m.sessionAgent[m.currentSessionID]
		stale := owner != "" && m.currentAgent != "" && owner != m.currentAgent
		if !stale {
			if usage, ok := m.sessionUsage[m.currentSessionID]; ok {
				return usage, true
			}
		}
	}

	// Fallback: search by current agent name.
	if m.currentAgent != "" {
		for sessionID, agentName := range m.sessionAgent {
			if agentName == m.currentAgent {
				if usage, ok := m.sessionUsage[sessionID]; ok {
					return usage, true
				}
			}
		}
	}

	// Fallback: if there's exactly one session, use it.
	if len(m.sessionUsage) == 1 {
		for _, usage := range m.sessionUsage {
			return usage, true
		}
	}
	return nil, false
}

// currentSessionTokens returns the token count for the current agent's session.
func (m *model) currentSessionTokens() (tokens int64, found bool) {
	if usage, ok := m.currentSessionUsage(); ok {
		return usage.InputTokens + usage.OutputTokens, true
	}
	return 0, false
}

// contextPercent returns a context usage percentage string for the current agent's session.
func (m *model) contextPercent() string {
	if usage, ok := m.currentSessionUsage(); ok && usage.ContextLimit > 0 {
		percent := (float64(usage.ContextLength) / float64(usage.ContextLimit)) * 100
		return fmt.Sprintf("%.0f%%", percent)
	}
	return ""
}

// getCurrentWorkingDirectory returns the current working directory with home directory replaced by ~/
func getCurrentWorkingDirectory() string {
	pwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Replace home directory with ~/
	if homeDir := paths.GetHomeDir(); homeDir != "" && strings.HasPrefix(pwd, homeDir) {
		pwd = "~" + pwd[len(homeDir):]
	}

	return pwd
}

// Update handles messages and updates the component state.
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := m.SetSize(msg.Width, msg.Height)
		return m, cmd
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		if m.mode == ModeVertical {
			// Update hover state for motion events before delegating to the
			// scrollview so the Subagents section can highlight the row under
			// the cursor. Mouse click/release are exempt because the chatPage
			// routes them through handleSidebarClickType for focus/open flow.
			if motion, ok := msg.(tea.MouseMotionMsg); ok {
				if m.updateSubagentHoverAt(motion.X, motion.Y) {
					m.invalidateCache()
				}
			}
			_, cmd := m.scrollview.Update(msg)
			return m, cmd
		}
		return m, nil
	case *runtime.TokenUsageEvent:
		m.SetTokenUsage(msg)
		return m, nil
	case *runtime.MCPInitStartedEvent:
		// Ignore if stream was cancelled (stale event from before cancellation)
		if m.streamCancelled {
			return m, nil
		}
		if !m.mcpInit {
			m.mcpInit = true
			m.invalidateCache()
			cmd := m.startSpinner()
			return m, cmd
		}
		return m, nil
	case *runtime.MCPInitFinishedEvent:
		if m.mcpInit {
			m.mcpInit = false
			m.invalidateCache()
			m.stopSpinner() // Will only stop if no other state needs it
		}
		return m, nil
	case *runtime.RAGIndexingStartedEvent:
		// Ignore if stream was cancelled (stale event from before cancellation)
		if m.streamCancelled {
			return m, nil
		}
		// Use composite key: "ragName/strategyName" to differentiate strategies within same RAG manager
		key := msg.RAGName + "/" + msg.StrategyName
		slog.Debug("Sidebar received RAG indexing started event", "rag", msg.RAGName, "strategy", msg.StrategyName, "key", key)
		state := &ragIndexingState{
			spinner: m.spinner.Reset(),
		}
		m.ragIndexing[key] = state
		m.invalidateCache()
		return m, state.spinner.Init()
	case *runtime.RAGIndexingProgressEvent:
		key := msg.RAGName + "/" + msg.StrategyName
		slog.Debug("Sidebar received RAG indexing progress event", "rag", msg.RAGName, "strategy", msg.StrategyName, "current", msg.Current, "total", msg.Total)
		if state, exists := m.ragIndexing[key]; exists {
			state.current = msg.Current
			state.total = msg.Total
			m.invalidateCache()
		}
		return m, nil
	case *runtime.RAGIndexingCompletedEvent:
		key := msg.RAGName + "/" + msg.StrategyName
		slog.Debug("Sidebar received RAG indexing completed event", "rag", msg.RAGName, "strategy", msg.StrategyName)
		if state, exists := m.ragIndexing[key]; exists {
			state.spinner.Stop()
			delete(m.ragIndexing, key)
			m.invalidateCache()
		}
		return m, nil
	case *runtime.ToolCallEvent:
		// Tool call started - ensure working agent is set
		if msg.AgentName != "" {
			m.workingAgent = msg.AgentName
			m.invalidateCache()
		}
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.SubAgentStartedEvent:
		m.recordSubAgentStart(msg)
		cmd := m.syncSubagentSpinner()
		return m, cmd
	case *runtime.SubAgentSentEvent:
		m.recordSubAgentSent(msg)
		cmd := m.syncSubagentSpinner()
		return m, cmd
	case *runtime.SubAgentUpdateEvent:
		m.recordSubAgentUpdate(msg)
		cmd := m.syncSubagentSpinner()
		return m, cmd
	case *runtime.ToolCallResponseEvent:
		// Tool response received - ensure working agent is set (in case stream events were missed)
		if msg.AgentName != "" {
			m.workingAgent = msg.AgentName
			m.invalidateCache()
		}
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.SessionTitleEvent:
		// Clear regenerating state now that title generation is done
		if m.titleRegenerating {
			m.titleRegenerating = false
			m.stopSpinner()
		}
		// Only update title and mark as generated if a non-empty title was provided
		if msg.Title != "" {
			m.sessionTitle = msg.Title
			m.titleGenerated = true
		}
		m.invalidateCache()
		return m, nil
	case *runtime.StreamStartedEvent:
		// Session-lifetime event: track the active session id and reset
		// transient cancel/idle flags. Per-turn spinner/working-agent
		// transitions now live in TurnStartedEvent / TurnEndedEvent.
		m.streamCancelled = false
		m.parentIdle = false
		m.currentSessionID = msg.SessionID
		// Kick off title-generation spinner once, at stream open time.
		if !m.titleGenerated {
			m.titleRegenerating = true
		}
		m.invalidateCache()
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.TurnStartedEvent:
		// Per-turn event: light up the working-agent row spinner now that a
		// new model turn is starting. Reset any parent-idle gate from the
		// previous turn.
		// Ignore if stream was cancelled (stale event).
		if m.streamCancelled {
			return m, nil
		}
		m.parentIdle = false
		m.workingAgent = msg.AgentName
		m.currentSessionID = msg.SessionID
		m.invalidateCache()
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.TurnEndedEvent:
		// Per-turn event: the model has finished its turn. Clear the working
		// agent row so the spinner stops between turns.
		m.workingAgent = ""
		m.invalidateCache()
		m.stopSpinner()
		return m, nil
	case *runtime.StreamStoppedEvent:
		m.workingAgent = ""
		m.parentIdle = false
		m.invalidateCache()
		m.stopSpinner() // Will only stop if no other state needs it
		return m, nil
	case *runtime.ParentIdleEvent:
		// Parent has finished its own turn and is only waiting on subagents.
		// Stop spinning the parent agent's sidebar row — it matches the
		// global working spinner which is also stopped in this state.
		// Subagent rows continue to animate via their own state.
		m.parentIdle = true
		m.invalidateCache()
		m.stopSpinner() // tick coordinator will re-check needsSpinner()
		return m, nil
	case *runtime.ParentResumeEvent:
		m.parentIdle = false
		m.invalidateCache()
		if m.needsSpinner() {
			cmd := m.startSpinner()
			return m, cmd
		}
		return m, nil
	case *runtime.AgentInfoEvent:
		cmd := m.SetAgentInfo(msg.AgentName, msg.Model, msg.Description)
		return m, cmd
	case *runtime.TeamInfoEvent:
		m.SetTeamInfo(msg.AvailableAgents)
		return m, nil
	case *runtime.AgentSwitchingEvent:
		m.SetAgentSwitching(msg.Switching)
		return m, nil
	case *runtime.ToolsetInfoEvent:
		// Ignore loading state if stream was cancelled (stale event from before cancellation)
		if m.streamCancelled && msg.Loading {
			return m, nil
		}
		m.SetToolsetInfo(msg.AvailableTools, msg.Loading)
		if msg.Loading {
			cmd := m.startSpinner()
			return m, cmd
		}
		m.stopSpinner() // Will only stop if no other state needs it
		return m, nil
	case messages.StreamCancelledMsg:
		// Clear parent-stream-local spinner state when ESC interrupts the
		// root turn. We also transition every non-terminal subagent to
		// StatusClosed: after a cancel the parent stream is gone, so any
		// subagent that has not already reported a terminal status cannot
		// continue updating via the event path. Leaving them in a running
		// state would keep the shared sidebar spinner ticking and display a
		// misleading "working" glyph on rows that are no longer alive.
		m.streamCancelled = true
		m.parentIdle = false
		m.workingAgent = ""
		m.toolsLoading = false
		m.mcpInit = false
		m.titleRegenerating = false
		// Mark all non-terminal subagents as closed so their rows stop
		// driving the spinner after the cancel.
		m.markAllAsTerminal()
		// Force-stop main spinner now that all spinner-driving state is clear.
		if m.spinnerActive {
			m.spinnerActive = false
			m.spinner.Stop()
		}
		// Stop and clear any in-flight RAG indexing spinners.
		for k, state := range m.ragIndexing {
			state.spinner.Stop()
			delete(m.ragIndexing, k)
		}
		m.invalidateCache()
		return m, nil
	case messages.SessionToggleChangedMsg:
		m.invalidateCache()
		return m, nil
	case messages.ThemeChangedMsg:
		// Theme changed - recreate spinners with new colors
		// The spinner pre-renders frames with colors, so we need to recreate it
		var cmds []tea.Cmd

		// Recreate main spinner
		wasActive := m.spinnerActive
		if wasActive {
			m.spinner.Stop()
		}
		m.spinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
		if wasActive {
			cmd := m.spinner.Init()
			m.spinnerActive = true
			cmds = append(cmds, cmd)
		}

		// Recreate all RAG indexing spinners
		for _, state := range m.ragIndexing {
			state.spinner.Stop()
			state.spinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
			cmds = append(cmds, state.spinner.Init())
		}

		m.invalidateCache() // Theme affects all styling
		return m, tea.Batch(cmds...)
	default:
		var cmds []tea.Cmd
		needsInvalidate := false

		// Update main spinner when any local state drives it. Notably, live
		// background subagents keep the spinner ticking even while the parent
		// stream is idle-waiting on them, so their RawFrame() actually
		// advances and they animate in the sidebar. The parent agent row
		// itself is gated by !m.parentIdle below; here we only care whether
		// *some* row still needs the shared frame ticker.
		if m.mcpInit || m.toolsLoading || (m.workingAgent != "" && !m.parentIdle) || m.titleRegenerating || m.liveSubagentCount() > 0 {
			oldFrame := m.spinner.RawFrame()
			model, cmd := m.spinner.Update(msg)
			m.spinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			if m.spinner.RawFrame() != oldFrame {
				needsInvalidate = true
			}
		}

		// Update each RAG indexing spinner
		for _, state := range m.ragIndexing {
			oldFrame := state.spinner.RawFrame()
			model, cmd := state.spinner.Update(msg)
			state.spinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			if state.spinner.RawFrame() != oldFrame {
				needsInvalidate = true
			}
		}

		// Invalidate cache when spinners update to show new animation frames
		if needsInvalidate {
			m.invalidateCache()
		}

		return m, tea.Batch(cmds...)
	}
}

// View renders the component
func (m *model) View() string {
	var content string
	if m.mode == ModeVertical {
		content = m.verticalView()
	} else {
		content = m.collapsedView()
	}

	// Apply horizontal padding
	if m.layoutCfg.PaddingLeft > 0 || m.layoutCfg.PaddingRight > 0 {
		leftPad := strings.Repeat(" ", m.layoutCfg.PaddingLeft)
		rightPad := strings.Repeat(" ", m.layoutCfg.PaddingRight)
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			lines[i] = leftPad + line + rightPad
		}
		content = strings.Join(lines, "\n")
	}

	return content
}

// starIndicator returns the star indicator string based on starred status.
// Returns empty string if session has no content yet.
func (m *model) starIndicator() string {
	if !m.sessionHasContent {
		return ""
	}
	return styles.StarIndicator(m.sessionStarred)
}

// computeCollapsedViewModel builds the view model for collapsed mode.
// This extracts data from the model and computes layout decisions,
// keeping the model's state separate from rendering concerns.
func (m *model) computeCollapsedViewModel(contentWidth int) CollapsedViewModel {
	star := m.starIndicator()

	var titleWithStar string
	switch {
	case m.editingTitle:
		titleWithStar = star + m.titleInput.View()
	case m.titleRegenerating:
		titleWithStar = star + m.spinner.View() + styles.MutedStyle.Render(" Generating title…")
	default:
		titleWithStar = star + m.sessionTitle
	}
	vm := CollapsedViewModel{
		TitleWithStar:    titleWithStar,
		ParentLine:       m.parentSessionLineCollapsed(),
		WorkingIndicator: m.workingIndicatorCollapsed(),
		WorkingDir:       m.workingDirectory,
		UsageSummary:     m.tokenUsageSummary(),
		SubagentSummary:  m.subagentSummaryCollapsed(),
		ContentWidth:     contentWidth,
	}

	titleWidth := lipgloss.Width(vm.TitleWithStar)
	wiWidth := lipgloss.Width(vm.WorkingIndicator)
	wdWidth := lipgloss.Width(vm.WorkingDir)
	usageWidth := lipgloss.Width(vm.UsageSummary)

	// Title and indicator fit on one line if:
	// - editing mode (input is constrained to fit in collapsed mode), OR
	// - no working indicator AND title fits, OR
	// - both fit together with gap
	vm.TitleAndIndicatorOnOneLine = m.editingTitle ||
		(vm.WorkingIndicator == "" && titleWidth <= contentWidth) ||
		(vm.WorkingIndicator != "" && titleWidth+minGap+wiWidth <= contentWidth)
	vm.WdAndUsageOnOneLine = wdWidth+minGap+usageWidth <= contentWidth

	return vm
}

// CollapsedHeight returns the number of lines needed for collapsed mode.
func (m *model) CollapsedHeight(outerWidth int) int {
	contentWidth := max(outerWidth-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, 1)
	return m.computeCollapsedViewModel(contentWidth).LineCount()
}

func (m *model) collapsedView() string {
	return RenderCollapsedView(m.computeCollapsedViewModel(m.contentWidth(false)))
}

func (m *model) verticalView() string {
	contentWidthNoScroll := m.contentWidth(false)

	// Use cached render if available and width hasn't changed
	if !m.cacheDirty && len(m.cachedLines) > 0 && m.cachedWidth == contentWidthNoScroll {
		return m.renderFromCache()
	}

	// Two-pass rendering: first check if scrollbar is needed
	// Pass 1: render without scrollbar to count lines
	lines := m.renderSections(contentWidthNoScroll)
	totalLines := len(lines)
	needsScrollbar := totalLines > m.height

	// Pass 2: if scrollbar needed, re-render with narrower content width
	if needsScrollbar {
		contentWidthWithScroll := m.contentWidth(true)
		lines = m.renderSections(contentWidthWithScroll)
	}

	// Cache the rendered lines
	m.cachedLines = lines
	m.cachedWidth = contentWidthNoScroll
	m.cachedNeedsScrollbar = needsScrollbar
	m.cacheDirty = false

	return m.renderFromCache()
}

// renderFromCache renders the sidebar from cached lines using the scrollview
// component which guarantees fixed-width output and a pinned scrollbar.
func (m *model) renderFromCache() string {
	// Compute the scrollview region width: content + gap + scrollbar (if needed)
	regionWidth := m.contentWidth(m.cachedNeedsScrollbar)
	if m.cachedNeedsScrollbar {
		regionWidth += m.layoutCfg.ScrollbarGap + scrollbar.Width
	}

	m.scrollview.SetSize(regionWidth, m.height)
	m.scrollview.SetContent(m.cachedLines, len(m.cachedLines))

	return m.scrollview.View()
}

// renderSections renders all sidebar sections and returns them as lines.
func (m *model) renderSections(contentWidth int) []string {
	var lines []string

	appendSection := func(section string) {
		if section != "" {
			lines = append(lines, strings.Split(section, "\n")...)
		}
	}

	appendSection(m.sessionInfo(contentWidth))
	appendSection(m.tokenUsage(contentWidth))
	appendSection(m.queueSection(contentWidth))

	// Subagents render above the Agents block on purpose: when a session is
	// actively delegating to live subagents they are the most operationally
	// relevant rows in the sidebar, so they should not be pushed below the
	// (often static) Agents list. When there are no subagents the section
	// returns "" and the visual layout is unchanged.
	subagentSectionStart := len(lines)
	appendSection(m.subagentSection(contentWidth))
	m.buildSubagentClickZones(subagentSectionStart, lines)

	// Track where agent entries start so we can detect clicks on agent names
	agentSectionStart := len(lines)
	appendSection(m.agentInfo(contentWidth))
	m.buildAgentClickZones(agentSectionStart, lines)

	appendSection(m.toolsetInfo(contentWidth))

	m.todoComp.SetSize(contentWidth)
	appendSection(strings.TrimSuffix(m.todoComp.Render(), "\n"))

	return lines
}

// ragStrategyInfo holds a parsed RAG strategy entry
type ragStrategyInfo struct {
	strategyName string
	state        *ragIndexingState
}

// groupedRAGIndexing returns RAG indexing states grouped and sorted by RAG name and strategy
func (m *model) groupedRAGIndexing() (ragNames []string, ragGroups map[string][]ragStrategyInfo) {
	ragGroups = make(map[string][]ragStrategyInfo)

	for key, state := range m.ragIndexing {
		parts := strings.Split(key, "/")
		if len(parts) == 2 {
			ragName := parts[0]
			ragGroups[ragName] = append(ragGroups[ragName], ragStrategyInfo{parts[1], state})
		}
	}

	// Sort RAG names and strategies for stable display
	ragNames = slices.Sorted(maps.Keys(ragGroups))
	for _, name := range ragNames {
		slices.SortFunc(ragGroups[name], func(a, b ragStrategyInfo) int {
			return strings.Compare(a.strategyName, b.strategyName)
		})
	}

	return ragNames, ragGroups
}

func (m *model) workingIndicator() string {
	var indicators []string

	if m.mcpInit {
		indicators = append(indicators, styles.ActiveStyle.Render(m.spinner.View()+" Initializing MCP servers…"))
	}

	ragNames, ragGroups := m.groupedRAGIndexing()
	for _, ragName := range ragNames {
		strategies := ragGroups[ragName]
		displayRagName := strings.ReplaceAll(ragName, "_", " ")

		// RAG source header
		header := "Indexing " + styles.BoldStyle.Render(displayRagName)
		indicators = append(indicators, styles.ActiveStyle.Render(header))

		// Each strategy with its spinner and progress
		for _, strategy := range strategies {
			displayStratName := strings.ReplaceAll(strategy.strategyName, "-", " ")
			progress := m.formatProgress(strategy.state)
			line := fmt.Sprintf("  %s %s%s", strategy.state.spinner.View(), styles.BoldStyle.Render(displayStratName), progress)
			indicators = append(indicators, line)
		}
	}

	if len(indicators) == 0 {
		return ""
	}

	return strings.Join(indicators, "\n")
}

// workingIndicatorCollapsed returns a single-line version of the working indicator for collapsed mode
func (m *model) workingIndicatorCollapsed() string {
	var labels []string

	if m.mcpInit {
		labels = append(labels, "Initializing MCP servers…")
	}

	ragNames, ragGroups := m.groupedRAGIndexing()
	for _, ragName := range ragNames {
		strategies := ragGroups[ragName]
		displayRagName := strings.ReplaceAll(ragName, "_", " ")

		labels = append(labels, "Indexing "+styles.BoldStyle.Render(displayRagName))

		for _, strategy := range strategies {
			displayStratName := strings.ReplaceAll(strategy.strategyName, "-", " ")
			progress := m.formatProgress(strategy.state)
			labels = append(labels, fmt.Sprintf("  • %s%s", styles.BoldStyle.Render(displayStratName), progress))
		}
	}

	if len(labels) == 0 {
		return ""
	}

	return styles.ActiveStyle.Render(m.spinner.View() + " " + strings.Join(labels, " | "))
}

func (m *model) formatProgress(state *ragIndexingState) string {
	if state.total > 0 {
		return fmt.Sprintf(" [%d/%d]", state.current, state.total)
	}
	return ""
}

// usageStats holds aggregated usage statistics across all sessions, computed
// once so both tokenUsage (vertical) and tokenUsageSummary (collapsed) can
// reuse the values without duplicating the computation logic.
type usageStats struct {
	tokens       int64
	contextPct   string
	totalCost    float64
	sessionCount int
}

func (m *model) computeUsageStats() usageStats {
	var s usageStats
	for _, usage := range m.sessionUsage {
		s.totalCost += usage.Cost
		s.sessionCount++
	}
	s.tokens, _ = m.currentSessionTokens()
	s.contextPct = m.contextPercent()
	return s
}

func (m *model) tokenUsage(contentWidth int) string {
	s := m.computeUsageStats()

	line := formatTokenCount(s.tokens)
	if s.contextPct != "" {
		line += " (" + s.contextPct + ")"
	}
	line += " " + styles.TabAccentStyle.Render("$"+formatCost(s.totalCost))
	if s.sessionCount > 1 {
		line += " " + styles.MutedStyle.Render(fmt.Sprintf("(%d sub-sessions)", s.sessionCount-1))
	}

	return m.renderTab("Token Usage", line, contentWidth)
}

// tokenUsageSummary returns a single-line summary for horizontal layout.
func (m *model) tokenUsageSummary() string {
	if len(m.sessionUsage) == 0 {
		return ""
	}

	s := m.computeUsageStats()

	parts := []string{"Tokens: " + formatTokenCount(s.tokens)}
	if s.sessionCount > 1 {
		if s.contextPct != "" {
			parts = append(parts, "Context: "+s.contextPct)
		}
		parts = append(parts, "Cost: $"+formatCost(s.totalCost), fmt.Sprintf("%d sub-sessions", s.sessionCount-1))
	} else {
		parts = append(parts, "Cost: $"+formatCost(s.totalCost))
		if s.contextPct != "" {
			parts = append(parts, "Context: "+s.contextPct)
		}
	}

	return strings.Join(parts, " | ")
}

func (m *model) sessionInfo(contentWidth int) string {
	star := m.starIndicator()

	var titleLine string
	switch {
	case m.editingTitle:
		// Width was pre-calculated in SetSize, just render
		titleLine = star + m.titleInput.View()
	case m.titleRegenerating:
		// Show spinner while regenerating title
		titleLine = star + m.spinner.View() + styles.MutedStyle.Render(" Generating title…")
	default:
		titleLine = star + m.sessionTitle
	}

	lines := []string{titleLine, ""}

	if m.workingDirectory != "" {
		lines = append(lines, styles.TabAccentStyle.Render("█")+styles.TabPrimaryStyle.Render(" "+m.workingDirectory))
	}

	if parentLine := m.parentSessionLine(contentWidth); parentLine != "" {
		// One blank line above the parent line so it detaches from the
		// working-directory row, matching the breathing room the Session
		// section uses elsewhere. The parent line is intentionally placed
		// at the bottom of the Session block so its visual position stays
		// consistent with the rest of the sidebar (informational rows
		// accumulate after the working directory).
		lines = append(lines, "", parentLine)
	}

	return m.renderTab("Session", strings.Join(lines, "\n"), contentWidth)
}

// queueSection renders the queued messages section
func (m *model) queueSection(contentWidth int) string {
	if len(m.queuedMessages) == 0 {
		return ""
	}

	maxMsgWidth := contentWidth - treePrefixWidth
	var lines []string

	for i, msg := range m.queuedMessages {
		// Determine prefix based on position
		var prefix string
		if i == len(m.queuedMessages)-1 {
			prefix = styles.MutedStyle.Render("└ ")
		} else {
			prefix = styles.MutedStyle.Render("├ ")
		}

		// Truncate message and add prefix
		truncated := toolcommon.TruncateText(msg, maxMsgWidth)
		lines = append(lines, prefix+truncated)
	}

	// Add hint for clearing
	lines = append(lines, styles.MutedStyle.Render("  Ctrl+X to clear"))

	title := fmt.Sprintf("Queue (%d)", len(m.queuedMessages))
	return m.renderTab(title, strings.Join(lines, "\n"), contentWidth)
}

// agentInfo renders the current agent information
func (m *model) agentInfo(contentWidth int) string {
	// Read current agent from session state so sidebar updates when agent is switched
	currentAgent := m.sessionState.CurrentAgentName()
	if currentAgent == "" {
		return ""
	}

	agentTitle := "Agent"
	if len(m.availableAgents) > 1 {
		agentTitle = "Agents"
	}
	if m.agentSwitching {
		agentTitle += " ↔"
	}

	var content strings.Builder
	for i, agent := range m.availableAgents {
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		isCurrent := agent.Name == currentAgent
		m.renderAgentEntry(&content, agent, isCurrent, i, contentWidth)
	}

	return m.renderTab(agentTitle, content.String(), contentWidth)
}

func (m *model) renderAgentEntry(content *strings.Builder, agent runtime.AgentDetails, isCurrent bool, index, contentWidth int) {
	agentStyle := styles.AgentAccentStyleFor(agent.Name)
	row := sidebarSessionRow{
		ID:          agent.Name,
		DisplayName: agent.Name,
		Description: agent.Description,
		Provider:    agent.Provider,
		Model:       agent.Model,
		IsCurrent:   isCurrent,
	}
	if index >= 0 && index < 9 {
		row.TrailingHint = styles.MutedStyle.Render(fmt.Sprintf("^%d", index+1))
	}
	if isCurrent {
		// The per-agent row spinner must mirror the global working spinner.
		// When ParentIdleEvent has suspended the parent turn (it's waiting on
		// subagents), we stop the global spinner — so the row spinner has to
		// stop too, otherwise the parent looks "busy" when it's not. The
		// Agents list represents the parent session, so a single !m.parentIdle
		// guard covers every entry (subagent rows animate separately).
		if m.workingAgent == agent.Name && !m.parentIdle {
			row.LeadingGlyph = m.spinner.RawFrame()
		} else {
			row.LeadingGlyph = "▶"
		}
	}

	header := renderSessionRowHeader(row, agentStyle)
	if row.TrailingHint != "" {
		nameWidth := lipgloss.Width(header)
		hintWidth := lipgloss.Width(row.TrailingHint)
		spaceWidth := max(contentWidth-nameWidth-hintWidth, 1)
		content.WriteString(header + strings.Repeat(" ", spaceWidth) + row.TrailingHint)
	} else {
		content.WriteString(header)
	}

	maxWidth := contentWidth - treePrefixWidth
	if desc := row.Description; desc != "" {
		content.WriteString("\n")
		content.WriteString(renderSessionRowMetaLine("├ ", desc, maxWidth))
	}
	content.WriteString("\n")
	content.WriteString(renderSessionRowMetaLine("├ ", "Provider: "+row.Provider, maxWidth))
	content.WriteString("\n")
	content.WriteString(renderSessionRowMetaLine("└ ", "Model: "+row.Model, maxWidth))
}

// buildAgentClickZones populates agentClickZones by scanning the rendered lines
// to find which lines belong to which agent. It relies on the structure produced
// by renderTab + agentInfo: a 2-line tab header, then agent blocks separated by
// visually blank lines. Each consecutive run of non-blank lines maps to the next
// agent in order. This avoids duplicating line-count logic from renderAgentEntry.
func (m *model) buildAgentClickZones(agentSectionStart int, lines []string) {
	if m.agentClickZones == nil {
		m.agentClickZones = make(map[int]string)
	}
	plans := m.agentRowPlans()
	buildRowClickZones(m.agentClickZones, agentSectionStart, lines, plans)
}

// agentRowPlans builds shared rowRenderPlan entries for the Agents section.
// The line layout MUST match what renderAgentEntry produces:
//
//	<name line>
//	[<description line>]
//	<provider line>
//	<model line>
func (m *model) agentRowPlans() []rowRenderPlan {
	if len(m.availableAgents) == 0 {
		return nil
	}
	plans := make([]rowRenderPlan, 0, len(m.availableAgents))
	for i, agent := range m.availableAgents {
		content := 1 // name
		if strings.TrimSpace(agent.Description) != "" {
			content++
		}
		content += 2 // provider + model
		sep := 1
		if i == len(m.availableAgents)-1 {
			sep = 0
		}
		plans = append(plans, rowRenderPlan{
			row: sidebarSessionRow{
				ID:           agent.Name,
				DisplayName:  agent.Name,
				Description:  agent.Description,
				Provider:     agent.Provider,
				Model:        agent.Model,
				IsAttachable: true,
			},
			contentLines:   content,
			separatorLines: sep,
		})
	}
	return plans
}

func sessionTreePrefix(depth int, isLast bool, ancestorsHaveMore []bool) string {
	if depth <= 0 {
		return ""
	}
	var b strings.Builder
	for _, hasMore := range ancestorsHaveMore {
		if hasMore {
			b.WriteString("│ ")
		} else {
			b.WriteString("  ")
		}
	}
	if isLast {
		b.WriteString("└ ")
	} else {
		b.WriteString("├ ")
	}
	return b.String()
}

func buildRowClickZones(zones map[int]string, sectionStart int, lines []string, plans []rowRenderPlan) {
	clear(zones)
	if len(plans) == 0 {
		return
	}
	const tabHeaderLines = 2 // tab title + TabStyle top padding
	lineIdx := sectionStart + tabHeaderLines
	for _, plan := range plans {
		if plan.row.ID == "" || plan.contentLines <= 0 {
			lineIdx += plan.separatorLines
			continue
		}
		for i := range plan.contentLines {
			if lineIdx+i < len(lines) {
				zones[lineIdx+i] = plan.row.ID
			}
		}
		lineIdx += plan.contentLines + plan.separatorLines
	}
}

func renderSessionRowHeader(row sidebarSessionRow, badgeStyle lipgloss.Style) string {
	leading := ""
	if row.LeadingGlyph != "" {
		leading = badgeStyle.Render(row.LeadingGlyph) + " "
	}
	return leading + badgeStyle.Render(row.DisplayName)
}

func renderSessionRowMetaLine(prefix, text string, maxWidth int) string {
	return styles.MutedStyle.Render(prefix) + toolcommon.TruncateText(text, maxWidth)
}

// toolsetInfo renders the current toolset status information
func (m *model) toolsetInfo(contentWidth int) string {
	var lines []string

	// Tools status line
	if toolsStatus := m.renderToolsStatus(); toolsStatus != "" {
		lines = append(lines, toolsStatus)
	}

	// Skills status line
	if m.availableSkills > 0 {
		lines = append(lines, m.renderSkillsStatus())
	}

	// Toggle indicators with shortcuts
	toggles := []struct {
		enabled  bool
		label    string
		shortcut string
	}{
		{m.sessionState.YoloMode(), "YOLO mode enabled", "^y"},
		{m.sessionState.HideToolResults(), "Tool output hidden", "^o"},
		{m.sessionState.SplitDiffView(), "Split Diff View", "/split-diff"},
	}

	for _, toggle := range toggles {
		if toggle.enabled {
			lines = append(lines, m.renderToggleIndicator(toggle.label, toggle.shortcut, contentWidth))
		}
	}

	if working := m.workingIndicator(); working != "" {
		lines = append(lines, working)
	}

	return m.renderTab("Tools", lipgloss.JoinVertical(lipgloss.Top, lines...), contentWidth)
}

// renderToolsStatus renders the tools available/loading status line
func (m *model) renderToolsStatus() string {
	if m.toolsLoading {
		if m.availableTools > 0 {
			return m.spinner.View() + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d tools available…", m.availableTools))
		}
		return m.spinner.View() + styles.TabPrimaryStyle.Render(" Loading tools…")
	}
	if m.availableTools > 0 {
		return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d tools available", m.availableTools))
	}
	return ""
}

// renderSkillsStatus renders the skills available status line
func (m *model) renderSkillsStatus() string {
	label := "skills available"
	if m.availableSkills == 1 {
		label = "skill available"
	}
	return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d %s", m.availableSkills, label))
}

// renderToggleIndicator renders a toggle status with its keyboard shortcut
func (m *model) renderToggleIndicator(label, shortcut string, contentWidth int) string {
	indicator := styles.TabAccentStyle.Render("✓") + styles.TabPrimaryStyle.Render(" "+label)
	shortcutStyled := lipgloss.PlaceHorizontal(contentWidth-lipgloss.Width(indicator), lipgloss.Right, styles.MutedStyle.Render(shortcut))
	return indicator + shortcutStyled
}

// SetSize sets the dimensions of the component
func (m *model) SetSize(width, height int) tea.Cmd {
	if m.width == width && m.height == height {
		return nil // Dimensions unchanged — skip cache invalidation
	}
	m.width = width
	m.height = height
	m.updateScrollviewPosition()
	m.updateTitleInputWidth()
	m.invalidateCache() // Width/height change affects layout
	return nil
}

// updateTitleInputWidth sets the title input viewport width.
// In vertical mode the input is wide enough to show the full text — the tab
// body's lipgloss Width wraps it visually. In collapsed mode the input is
// constrained to the single available line so it scrolls horizontally instead.
func (m *model) updateTitleInputWidth() {
	if m.mode == ModeCollapsed {
		starWidth := lipgloss.Width(m.starIndicator())
		inputWidth := m.contentWidth(false) - starWidth
		m.titleInput.SetWidth(max(10, inputWidth))
	} else {
		m.titleInput.SetWidth(m.titleInput.CharLimit)
	}
}

// SetPosition sets the absolute position of the component on screen
func (m *model) SetPosition(x, y int) tea.Cmd {
	m.xPos = x
	m.yPos = y
	m.updateScrollviewPosition()
	return nil
}

// updateScrollviewPosition updates the scrollview's position based on sidebar position and layout.
func (m *model) updateScrollviewPosition() {
	// The scrollview region starts after left padding.
	m.scrollview.SetPosition(m.xPos+m.layoutCfg.PaddingLeft, m.yPos)
}

// GetSize returns the current dimensions
func (m *model) GetSize() (width, height int) {
	return m.width, m.height
}

func (m *model) SetMode(mode Mode) {
	if mode == ModeCollapsed {
		m.subagentClickZones = nil
	}
	m.mode = mode
	m.invalidateCache()
}

func (m *model) renderTab(title, content string, contentWidth int) string {
	return tab.Render(title, content, contentWidth)
}

// metrics computes the layout metrics for the current render.
// scrollbarVisible should be true if the scrollbar will be shown.
func (m *model) metrics(scrollbarVisible bool) Metrics {
	return m.layoutCfg.Compute(m.width, scrollbarVisible)
}

// contentWidth returns the width available for content in the current mode.
// For horizontal mode, scrollbar is never shown.
// For vertical mode, this is a preliminary estimate; actual scrollbar visibility
// is determined during render.
func (m *model) contentWidth(scrollbarVisible bool) int {
	return m.metrics(scrollbarVisible).ContentWidth
}

// IsCollapsed returns whether the sidebar is collapsed
func (m *model) IsCollapsed() bool {
	return m.collapsed
}

// ToggleCollapsed toggles the collapsed state of the sidebar.
// When expanding, if the preferred width is below minimum (e.g., after drag-to-collapse),
// it resets to the default width.
func (m *model) ToggleCollapsed() {
	m.collapsed = !m.collapsed
	if !m.collapsed && m.preferredWidth < MinWidth {
		m.preferredWidth = DefaultWidth
	}
}

// SetCollapsed sets the collapsed state directly.
// When expanding, if the preferred width is below minimum (e.g., after drag-to-collapse),
// it resets to the default width.
func (m *model) SetCollapsed(collapsed bool) {
	m.collapsed = collapsed
	if !collapsed && m.preferredWidth < MinWidth {
		m.preferredWidth = DefaultWidth
	}
}

// GetPreferredWidth returns the user's preferred width
func (m *model) GetPreferredWidth() int {
	return m.preferredWidth
}

// SetPreferredWidth sets the user's preferred width
func (m *model) SetPreferredWidth(width int) {
	m.preferredWidth = width
}

// ClampWidth ensures width is within valid bounds for the given window inner width
func (m *model) ClampWidth(width, windowInnerWidth int) int {
	maxWidth := min(int(float64(windowInnerWidth)*MaxWidthPercent), windowInnerWidth-20)
	return max(MinWidth, min(width, maxWidth))
}

// HandleTitleClick handles a click on the title area and returns true if
// edit mode should start (on double-click).
func (m *model) HandleTitleClick() bool {
	now := time.Now()
	if now.Sub(m.lastTitleClickTime) < styles.DoubleClickThreshold {
		m.lastTitleClickTime = time.Time{} // Reset to prevent triple-click
		return true
	}
	m.lastTitleClickTime = now
	return false
}

// BeginTitleEdit starts inline editing of the session title
func (m *model) BeginTitleEdit() {
	m.editingTitle = true
	m.titleInput.SetValue(m.sessionTitle)
	m.updateTitleInputWidth()
	m.titleInput.Focus()
	m.titleInput.CursorEnd()
	m.invalidateCache()
}

// IsEditingTitle returns true if the title is being edited
func (m *model) IsEditingTitle() bool {
	return m.editingTitle
}

// CommitTitleEdit commits the current title edit and returns the new title
func (m *model) CommitTitleEdit() string {
	newTitle := strings.TrimSpace(m.titleInput.Value())
	if newTitle != "" {
		m.sessionTitle = newTitle
	}
	m.editingTitle = false
	m.titleInput.Blur()
	m.invalidateCache()
	return m.sessionTitle
}

// CancelTitleEdit cancels the current title edit
func (m *model) CancelTitleEdit() {
	m.editingTitle = false
	m.titleInput.Blur()
	m.invalidateCache()
}

// UpdateTitleInput passes a key message to the title input
func (m *model) UpdateTitleInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.titleInput, cmd = m.titleInput.Update(msg)
	m.invalidateCache() // Input changes affect rendering
	return cmd
}
