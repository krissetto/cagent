package sidebar

import (
	"context"
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

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/gitbranch"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
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

// SectionVisibility controls which optional sidebar sections are rendered.
// The zero value shows everything.
type SectionVisibility struct {
	HideUsage  bool
	HideAgents bool
	HideTools  bool
	HideTodos  bool
}

// Model represents a sidebar component
type Model interface {
	layout.Model
	layout.Sizeable
	layout.Positionable

	SetTokenUsage(event *runtime.TokenUsageEvent)
	SetTodos(result *tools.ToolCallResult) error
	SetMode(mode Mode)
	// SetMirroredPadding swaps the horizontal edge padding so the sidebar hugs
	// the terminal edge when rendered on the left of the chat.
	SetMirroredPadding(mirrored bool)
	SetAgentInfo(agentName, model, description string) tea.Cmd
	SetTeamInfo(availableAgents []runtime.AgentDetails)
	SetAgentSwitching(switching bool)
	SetToolsetInfo(availableTools int, loading bool)
	SetSkillsInfo(availableSkills int)
	SetSessionStarred(starred bool)
	SetQueuedMessages(messages ...string)
	// SetSectionVisibility controls which optional sidebar sections are rendered.
	SetSectionVisibility(v SectionVisibility)
	// SetSectionGap sets the number of blank lines between sidebar sections.
	SetSectionGap(lines int)
	GetSize() (width, height int)
	LoadFromSession(sess *session.Session)
	// ResetStreamTracking clears the active-stream stack so a new top-level run
	// starts from a clean slate, even if a previous run's stream events were
	// left unbalanced (e.g. cancelled without a StreamCancelledMsg).
	ResetStreamTracking()
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
	// ClearSubagentHover resets the hovered subagent row (mouse left the sidebar).
	ClearSubagentHover()
	// SetSubagentContext marks this sidebar as belonging to a tab attached to
	// a subagent's sub-session: the Subagents section roots at that subagent's
	// node and a clickable "parent: <agent>" line links back to the parent tab.
	SetSubagentContext(root subagent.NodeID, parentAgent, parentSessionID string)
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
	width              int
	height             int
	xPos               int                       // absolute x position on screen
	yPos               int                       // absolute y position on screen
	layoutCfg          LayoutConfig              // layout configuration for spacing
	sessionUsage       map[string]*runtime.Usage // sessionID -> latest usage snapshot
	todoComp           *todotool.SidebarComponent
	ragIndexing        map[string]*ragIndexingState // strategy name -> indexing state
	spinner            spinner.Spinner
	spinnerActive      bool // true when spinner is registered with animation coordinator
	mode               Mode
	sessionTitle       string
	sessionStarred     bool
	sessionHasContent  bool // true when session has been used (has messages)
	currentAgent       string
	agentModel         string
	agentDescription   string
	availableAgents    []runtime.AgentDetails
	agentSwitching     bool
	availableTools     int
	availableSkills    int
	toolsLoading       bool // true when more tools may still be loading
	sessionState       *service.SessionState
	workingAgent       string   // Name of the agent currently working (empty if none)
	sessionStack       []string // Active stream session IDs; the top is the active (deepest) session
	rootSessionID      string   // Main (top-level) session, shown when no stream is active
	scrollview         *scrollview.Model
	workingDirectory   string
	gitBranchName      string                  // current git branch, empty if not in a repo
	queuedMessages     []string                // Truncated preview of queued messages
	subagentNodes      []subagent.NodeSnapshot // Live async subagent swarm (children of the session root)
	subagentSpinner    spinner.Spinner         // Shared spinner frame for running subagent rows
	subagentSpinnerOn  bool                    // true when subagentSpinner is registered with the animation coordinator
	hoveredSubagent    subagent.NodeID         // node under the mouse cursor, "" when none
	streamCancelled    bool                    // true after ESC cancel until next StreamStartedEvent
	compacting         bool                    // true while a session compaction runs (started → completed)
	collapsed          bool                    // true when sidebar is collapsed
	titleRegenerating  bool                    // true when title is being regenerated by AI
	titleGenerated     bool                    // true once a title has been generated or set (hides pencil until then)
	preferredWidth     int                     // user's preferred width (persisted across collapse/expand)
	editingTitle       bool                    // true when inline title editing is active
	titleInput         textinput.Model
	lastTitleClickTime time.Time         // for double-click detection on title
	sectionVisibility  SectionVisibility // which optional sections are rendered
	sectionGap         int               // blank lines between sections in vertical mode

	ctx func() context.Context

	// Render cache to avoid re-rendering sections on every frame during scroll
	cachedLines          []string // Cached rendered lines
	cachedWidth          int      // Width used for cached render
	cachedNeedsScrollbar bool     // Whether scrollbar is needed for cached render
	cacheDirty           bool     // True when cache needs rebuild
	// sectionCache reuses each section's rendered string across animation-only
	// frames (a spinner advancing, hover moving between subagent rows): only
	// sections whose content can change on such a frame are re-rendered, the
	// rest are served as-is. Cleared on full invalidations and width changes.
	sectionCache      map[string]string
	sectionCacheWidth int
	layoutDirty       bool // True when a change may alter line count/scrollbar visibility (not just an animation frame)

	// Agent click zones: maps content line index to agent name for click detection
	agentClickZones map[int]string // content line -> agent name
	// agentLineOwners records, per rendered agent-section body line, which agent
	// emitted it (empty for blank separators). It is produced during agentInfo
	// rendering so click zones can be registered explicitly rather than inferred
	// from blank-line heuristics.
	agentLineOwners []string

	// Subagent hover zones mirror the agent click zone mechanism: per rendered
	// subagent-section body line, the node it belongs to, and the resulting
	// content-line → node map used for mouse-motion hover detection.
	subagentLineNodes []subagent.NodeID
	subagentHoverZone map[int]subagent.NodeID

	// Attached-subagent context: when this sidebar belongs to a tab attached
	// to a subagent's sub-session, the Subagents section roots at that
	// subagent's node and a clickable "parent: <agent>" line links back to the
	// parent tab.
	subagentRootNode subagent.NodeID
	parentAgent      string
	parentSessionID  string
	parentLineZone   int  // content line of the parent line, -1 when absent
	hoveredParent    bool // mouse over the parent line
	// subagentRowOffset is the number of section body lines before the first
	// child row (the parent line + spacer on attached tabs).
	subagentRowOffset int
}

// New creates a new sidebar bound to the given session state.
func New(ctx context.Context, sessionState *service.SessionState) Model {
	ti := textinput.New()
	ti.Placeholder = "Session title"
	ti.CharLimit = 50
	ti.Prompt = "" // No prompt to maximize usable width in collapsed sidebar

	wd, branch := getCurrentWorkingDirectory()

	m := &model{
		parentLineZone:  -1,
		ctx:             func() context.Context { return context.WithoutCancel(ctx) },
		width:           20,
		layoutCfg:       DefaultLayoutConfig(),
		height:          24,
		sessionUsage:    make(map[string]*runtime.Usage),
		todoComp:        todotool.NewSidebarComponent(),
		spinner:         spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle),
		subagentSpinner: spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle),
		sessionTitle:    "New session",
		ragIndexing:     make(map[string]*ragIndexingState),
		sessionState:    sessionState,
		scrollview: scrollview.New(
			scrollview.WithWheelStep(1),
			scrollview.WithKeyMap(nil), // Sidebar has no keyboard scroll — only mouse
		),
		workingDirectory: wd,
		gitBranchName:    branch,
		preferredWidth:   DefaultWidth,
		sectionGap:       defaultSectionGap,
		titleInput:       ti,
		cacheDirty:       true, // Initial render needed
		layoutDirty:      true, // First render must probe scrollbar visibility
	}
	return m
}

func (m *model) Init() tea.Cmd {
	return nil
}

// needsSpinner returns true if any spinner-driving state is active.
func (m *model) needsSpinner() bool {
	return m.workingAgent != "" || m.toolsLoading || m.titleRegenerating || m.compacting
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

// invalidateCache marks the sidebar render cache as dirty so it will be rebuilt
// on the next View(). Use this for changes that may alter the rendered content
// AND its line layout (todos, sizing, agents, theme, …): the next View()
// re-probes scrollbar visibility via the two-pass render.
func (m *model) invalidateCache() {
	m.cacheDirty = true
	m.layoutDirty = true
	m.sectionCache = nil
}

// invalidateAnimation marks the cache dirty for an animation-only change, i.e. a
// spinner advancing one frame. Spinner glyphs are fixed (single-cell) width, so
// the line count and scrollbar visibility cannot change; the next View() can
// therefore skip the scrollbar-probe pass and render the sections only once.
func (m *model) invalidateAnimation() {
	m.cacheDirty = true
}

func (m *model) SetTokenUsage(event *runtime.TokenUsageEvent) {
	if event == nil || event.Usage == nil || event.SessionID == "" || event.AgentName == "" {
		return
	}

	// Store/replace by session ID (each event has cumulative totals for that session)
	usage := *event.Usage
	m.sessionUsage[event.SessionID] = &usage

	// Record the per-agent snapshot in the shared session state so the
	// agent roster and the agent-details dialog can show per-agent context
	// usage. Background agent tasks reach this path too: their usage
	// events are forwarded out-of-band by the runtime.
	m.sessionState.SetAgentUsage(event.AgentName, usage)

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

// SetSectionVisibility controls which optional sidebar sections are rendered.
func (m *model) SetSectionVisibility(v SectionVisibility) {
	if m.sectionVisibility == v {
		return
	}
	m.sectionVisibility = v
	m.invalidateCache()
}

// SetSectionGap sets the number of blank lines between sidebar sections.
func (m *model) SetSectionGap(lines int) {
	if lines < 0 {
		lines = 0
	}
	if m.sectionGap == lines {
		return
	}
	m.sectionGap = lines
	m.invalidateCache()
}

// SetSubagentContext marks this sidebar as attached to a subagent's session.
func (m *model) SetSubagentContext(root subagent.NodeID, parentAgent, parentSessionID string) {
	m.subagentRootNode = root
	m.parentAgent = parentAgent
	m.parentSessionID = parentSessionID
	m.invalidateCache()
}

// SetSubagentTree replaces the rendered subagent swarm. Each level is ordered
// most-recently-spawned first (subtree structure preserved). Returns a command
// to start the shared row spinner when any subagent is running.
func (m *model) SetSubagentTree(snapshot subagent.Snapshot) tea.Cmd {
	// A live tree that has no root for this view says nothing about it: at
	// process start the bridge publishes the (empty) initial snapshot, which
	// must not wipe a view just restored from the session store. Only
	// snapshots that contain the view's root are authoritative.
	if rid := m.treeRootID(); rid != "" && len(m.subagentNodes) > 0 && !hasTreeRoot(snapshot, rid) {
		return m.syncSubagentSpinner()
	}
	nodes := sortSubagentNodes(m.rootedChildren(snapshot))
	if !sameSubagentNodes(m.subagentNodes, nodes) {
		m.subagentNodes = nodes
		m.invalidateCache()
	}
	return m.syncSubagentSpinner()
}

// treeRootID returns the tree node this sidebar's swarm view roots at: the
// attached subagent's node, or the session's synthetic root.
func (m *model) treeRootID() subagent.NodeID {
	if m.subagentRootNode != "" {
		return m.subagentRootNode
	}
	if m.rootSessionID != "" {
		return subagent.SessionRootID(m.rootSessionID)
	}
	return ""
}

// hasTreeRoot reports whether the snapshot contains the given node.
func hasTreeRoot(snapshot subagent.Snapshot, rootID subagent.NodeID) bool {
	if rootID == "" {
		return false
	}
	return findNodeSnapshot(snapshot.Nodes, rootID) != nil
}

// findNodeSnapshot depth-first searches nodes for the given id.
func findNodeSnapshot(nodes []subagent.NodeSnapshot, id subagent.NodeID) *subagent.NodeSnapshot {
	for i := range nodes {
		if nodes[i].Node.ID == id {
			return &nodes[i]
		}
		if found := findNodeSnapshot(nodes[i].Children, id); found != nil {
			return found
		}
	}
	return nil
}

// rootedChildren returns the subtree this sidebar renders: the children of
// the attached subagent's node, or the session root's children.
func (m *model) rootedChildren(snapshot subagent.Snapshot) []subagent.NodeSnapshot {
	if m.subagentRootNode != "" {
		if node := findNodeSnapshot(snapshot.Nodes, m.subagentRootNode); node != nil {
			return node.Children
		}
		return nil
	}
	return flattenRootChildren(snapshot, m.rootSessionID)
}

// syncSubagentSpinner starts/stops the shared subagent spinner based on
// whether any node is currently running.
func (m *model) syncSubagentSpinner() tea.Cmd {
	running := hasRunningSubagent(m.subagentNodes)
	switch {
	case running && !m.subagentSpinnerOn:
		m.subagentSpinnerOn = true
		return m.subagentSpinner.Init()
	case !running && m.subagentSpinnerOn:
		m.subagentSpinnerOn = false
		m.subagentSpinner.Stop()
	}
	return nil
}

func hasRunningSubagent(nodes []subagent.NodeSnapshot) bool {
	for i := range nodes {
		if isActiveSubagentState(nodes[i].Node.State) || hasRunningSubagent(nodes[i].Children) {
			return true
		}
	}
	return false
}

// isActiveSubagentState reports whether the node should show an animated
// spinner (work in progress, from the user's point of view).
func isActiveSubagentState(s subagent.NodeState) bool {
	return s == subagent.NodeStarting || s == subagent.NodeRunning
}

// flattenRootChildren lifts the subagents out from under their synthetic
// session-root nodes. When sessionID is set, only that session's subtree is
// kept: the runtime's live tree can carry roots for other sessions run in the
// same process.
func flattenRootChildren(snapshot subagent.Snapshot, sessionID string) []subagent.NodeSnapshot {
	var nodes []subagent.NodeSnapshot
	for _, root := range snapshot.Nodes {
		if sessionID != "" && root.Node.Parent == "" && root.Node.ID != subagent.SessionRootID(sessionID) {
			continue
		}
		if len(root.Children) > 0 {
			nodes = append(nodes, root.Children...)
		} else if root.Node.Parent != "" {
			nodes = append(nodes, root)
		}
	}
	return nodes
}

// sortSubagentNodes orders siblings most-recently-spawned first, recursively,
// without mutating the shared snapshot slices.
func sortSubagentNodes(nodes []subagent.NodeSnapshot) []subagent.NodeSnapshot {
	if len(nodes) == 0 {
		return nil
	}
	sorted := make([]subagent.NodeSnapshot, len(nodes))
	for i := range nodes {
		sorted[i] = nodes[i]
		sorted[i].Children = sortSubagentNodes(nodes[i].Children)
	}
	slices.SortStableFunc(sorted, func(a, b subagent.NodeSnapshot) int {
		return b.Node.CreatedAt.Compare(a.Node.CreatedAt)
	})
	return sorted
}

func sameSubagentNodes(a, b []subagent.NodeSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Node.ID != b[i].Node.ID || a[i].Node.State != b[i].Node.State || !sameSubagentNodes(a[i].Children, b[i].Children) {
			return false
		}
	}
	return true
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
	ClickTitle          // Click on the title area (use double-click to edit)
	ClickWorkingDir     // Click on the working directory line
	ClickAgent          // Click on an agent name in the sidebar
	ClickSubagent       // Click on a subagent row in the swarm section (payload: node id)
	ClickSubagentParent // Click on the "parent: <agent>" line of an attached tab (payload: parent session id)
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
		// In collapsed mode, title starts at line 0
		titleLines := m.titleLineCount()

		// Check if click is within the title area (line 0 to titleLines-1)
		if y >= 0 && y < titleLines {
			// Check if click is on the star (first line only, first few chars)
			if y == 0 && m.sessionHasContent && adjustedX <= starClickWidth {
				return ClickStar, ""
			}
			// Click is on title area (for double-click to edit)
			if m.titleGenerated && !m.editingTitle {
				return ClickTitle, ""
			}
		}

		// In collapsed mode, working dir line follows the title section.
		vm := m.computeCollapsedViewModel(m.contentWidth(false))
		wdStartY := vm.titleSectionLines()
		wdLines := linesNeeded(lipgloss.Width(vm.WorkingDir), vm.ContentWidth)
		if m.workingDirectory != "" && y >= wdStartY && y < wdStartY+wdLines {
			return ClickWorkingDir, ""
		}

		return ClickNone, ""
	}

	// In vertical mode, the title starts at verticalStarY
	scrollOffset := m.scrollview.ScrollOffset()
	contentY := y + scrollOffset // Convert viewport Y to content Y
	titleLines := m.titleLineCount()

	// Check if click is within the title area
	if contentY >= verticalStarY && contentY < verticalStarY+titleLines {
		// Check if click is on the star (first line only, first few chars)
		if contentY == verticalStarY && m.sessionHasContent && adjustedX <= starClickWidth {
			return ClickStar, ""
		}
		// Click is on title area (for double-click to edit)
		if m.titleGenerated && !m.editingTitle {
			return ClickTitle, ""
		}
	}

	// Working dir is at: verticalStarY + titleLines (title) + 1 (empty separator)
	if m.workingDirectory != "" && contentY == verticalStarY+titleLines+1 {
		return ClickWorkingDir, ""
	}

	// Check if click is on an agent name
	if agentName, ok := m.agentClickZones[contentY]; ok {
		return ClickAgent, agentName
	}

	// The "parent: <agent>" line of an attached subagent tab links back to
	// the parent's tab.
	if m.parentLineZone >= 0 && contentY == m.parentLineZone && m.parentSessionID != "" {
		return ClickSubagentParent, m.parentSessionID
	}

	// A subagent swarm row opens (or focuses) a tab attached to its session.
	if id, ok := m.subagentHoverZone[contentY]; ok && id != "" {
		return ClickSubagent, string(id)
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

	// The restored session is the main session until a stream starts. A freshly
	// loaded session has no in-flight streams, so clear any stale stack entries.
	m.rootSessionID = sess.ID
	m.sessionStack = nil

	// Restore the subagent swarm view. rootSessionID scopes live snapshots
	// to this session's subtree (the runtime tree may hold other sessions').
	// States are authoritative: the App reloads the tree through the runtime,
	// which adopts resumable subagents as idle and marks unresumable ones
	// stopped.
	m.hoveredSubagent = ""
	m.subagentNodes = nil
	if snap := sess.GetSubagentTree(); snap != nil {
		m.SetSubagentTree(*snap)
	}
	m.invalidateCache()

	// Load session title
	if sess.Title != "" {
		m.sessionTitle = sess.Title
		m.titleGenerated = true // Mark as generated since session already has a title
	}

	// Load starred status
	m.sessionStarred = sess.Starred

	// Load working directory from session
	if sess.WorkingDir != "" {
		m.workingDirectory, m.gitBranchName = formatWorkingDirectory(sess.WorkingDir)
	}

	// Session has content if it has messages or token usage
	m.sessionHasContent = len(sess.Messages) > 0 || sess.InputTokens > 0 || sess.OutputTokens > 0

	m.invalidateCache()
}

// ResetStreamTracking clears the active-stream stack. It is called when a new
// top-level run begins so leaked entries from a previous run that ended without
// a balanced StreamStoppedEvent (e.g. a context cancel without a
// StreamCancelledMsg) cannot pile up and pin the panel to a stale sub-session.
// rootSessionID is preserved so the idle display stays valid until the next
// stream starts.
func (m *model) ResetStreamTracking() {
	if len(m.sessionStack) == 0 {
		return
	}
	m.sessionStack = nil
	m.invalidateCache()
}

// activeSessionID returns the session whose usage the sidebar should display:
// the deepest currently-running stream, or the main session when idle. It is
// derived from the stream stack rather than the (rapidly toggling) current
// agent, so the displayed totals stay stable while a sub-agent runs instead of
// flickering between the parent and sub-session.
func (m *model) activeSessionID() string {
	if n := len(m.sessionStack); n > 0 {
		return m.sessionStack[n-1]
	}
	return m.rootSessionID
}

// activeSessionUsage returns the usage snapshot for the active session.
func (m *model) activeSessionUsage() (*runtime.Usage, bool) {
	if id := m.activeSessionID(); id != "" {
		if usage, ok := m.sessionUsage[id]; ok {
			return usage, true
		}
	}

	// Fallback: if there's exactly one session, use it (e.g. restored from
	// persistence before any stream has started).
	if len(m.sessionUsage) == 1 {
		for _, usage := range m.sessionUsage {
			return usage, true
		}
	}
	return nil, false
}

// activeSessionTokens returns the token count for the active session.
func (m *model) activeSessionTokens() (tokens int64, found bool) {
	if usage, ok := m.activeSessionUsage(); ok {
		return usage.InputTokens + usage.OutputTokens, true
	}
	return 0, false
}

// contextPercent returns a context usage percentage string for the active session.
func (m *model) contextPercent() string {
	if usage, ok := m.activeSessionUsage(); ok {
		return usageContextPercent(usage)
	}
	return ""
}

// usageContextPercent formats a usage snapshot's context fill as "N%", or ""
// when the snapshot is missing or its context limit is unknown.
func usageContextPercent(usage *runtime.Usage) string {
	if usage == nil || usage.ContextLimit <= 0 {
		return ""
	}
	percent := (float64(usage.ContextLength) / float64(usage.ContextLimit)) * 100
	return fmt.Sprintf("%.0f%%", percent)
}

// agentContextPercent returns the latest known context usage percentage for
// the named agent ("N%"), or "" when the agent has not run yet or its
// context limit is unknown.
func (m *model) agentContextPercent(agentName string) string {
	if usage, ok := m.sessionState.AgentUsage(agentName); ok {
		return usageContextPercent(&usage)
	}
	return ""
}

// agentContextGaugeLevel returns the warning level of the named agent's
// context gauge, [styles.ContextGaugeNormal] when the agent has not run yet.
func (m *model) agentContextGaugeLevel(agentName string) styles.ContextGaugeLevel {
	if usage, ok := m.sessionState.AgentUsage(agentName); ok {
		return usageGaugeLevel(&usage)
	}
	return styles.ContextGaugeNormal
}

// usageGaugeLevel classifies a usage snapshot's context fill against the
// compaction threshold it carries (0 falls back to the package default).
func usageGaugeLevel(usage *runtime.Usage) styles.ContextGaugeLevel {
	if usage == nil || usage.ContextLimit <= 0 {
		return styles.ContextGaugeNormal
	}
	fill := float64(usage.ContextLength) / float64(usage.ContextLimit)
	return styles.ContextGaugeLevelFor(fill, usage.CompactionThreshold)
}

// contextGaugeStyle picks the style for a context gauge reading: the
// section's normal style until usage approaches the compaction threshold,
// then the shared warning/critical colors.
func contextGaugeStyle(level styles.ContextGaugeLevel, normal lipgloss.Style) lipgloss.Style {
	switch level {
	case styles.ContextGaugeCritical:
		return styles.ErrorStyle
	case styles.ContextGaugeWarning:
		return styles.WarningStyle
	default:
		return normal
	}
}

// formatWorkingDirectory formats a raw directory path for display,
// replacing the home prefix with ~/. Returns the display path and the
// current git branch (empty if not in a repo).
func formatWorkingDirectory(rawDir string) (display, branch string) {
	if rawDir == "" {
		return "", ""
	}
	return pathx.ShortenHome(rawDir), gitbranch.Current(rawDir)
}

// getCurrentWorkingDirectory returns the current working directory with home directory
// replaced by ~/, along with the current git branch name.
func getCurrentWorkingDirectory() (string, string) {
	pwd, err := os.Getwd()
	if err != nil {
		return "", ""
	}

	return formatWorkingDirectory(pwd)
}

// workingDirWithBranch returns the working directory path with the git branch
// appended in muted style, suitable for rendering in the sidebar.
func (m *model) workingDirWithBranch() string {
	if m.workingDirectory == "" {
		return ""
	}
	result := m.workingDirectory
	if m.gitBranchName != "" {
		result += styles.MutedStyle.Render(" (" + m.gitBranchName + ")")
	}
	return result
}

// workingDirLine renders the working directory with the sidebar's accent
// block, shared by the vertical Session tab and the collapsed band.
func (m *model) workingDirLine() string {
	if m.workingDirectory == "" {
		return ""
	}
	return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(" "+m.workingDirWithBranch())
}

// Update handles messages and updates the component state.
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := m.SetSize(msg.Width, msg.Height)
		return m, cmd
	case tea.MouseMotionMsg:
		if m.mode != ModeVertical {
			return m, nil
		}
		_, cmd := m.scrollview.Update(msg)
		m.updateSubagentHover(msg.Y)
		return m, cmd
	case tea.MouseClickMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		if m.mode == ModeVertical {
			_, cmd := m.scrollview.Update(msg)
			return m, cmd
		}
		return m, nil
	case *runtime.TokenUsageEvent:
		m.SetTokenUsage(msg)
		return m, nil
	case *runtime.SessionCompactionEvent:
		m.compacting = msg.Status == "started"
		m.invalidateCache()
		if m.compacting {
			cmd := m.startSpinner()
			return m, cmd
		}
		m.stopSpinner()
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
		// New stream starting - reset cancelled flag and enable spinner
		m.streamCancelled = false
		m.workingAgent = msg.AgentName
		// Track the active session via a stack: the outermost stream owns the
		// main session; nested sub-agent streams are pushed on top so their
		// usage is shown while they run, then popped when they stop.
		if len(m.sessionStack) == 0 {
			m.rootSessionID = msg.SessionID
		}
		m.sessionStack = append(m.sessionStack, msg.SessionID)
		// If title hasn't been generated yet, show the title generation spinner
		if !m.titleGenerated {
			m.titleRegenerating = true
		}
		m.invalidateCache()
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.StreamStoppedEvent:
		m.workingAgent = ""
		if n := len(m.sessionStack); n > 0 {
			m.sessionStack = m.sessionStack[:n-1]
		}
		m.invalidateCache()
		m.stopSpinner() // Will only stop if no other state needs it
		return m, nil
	case *runtime.AgentInfoEvent:
		cmd := m.SetAgentInfo(msg.AgentName, msg.Model, msg.Description)
		return m, cmd
	case *runtime.TeamInfoEvent:
		m.SetTeamInfo(msg.AvailableAgents)
		return m, nil
	case *runtime.SubagentTreeEvent:
		cmd := m.SetSubagentTree(msg.Snapshot)
		return m, cmd
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
		// Clear all spinner-driving state when stream is cancelled via ESC
		m.streamCancelled = true
		m.workingAgent = ""
		m.sessionStack = nil
		m.toolsLoading = false
		m.titleRegenerating = false
		m.compacting = false
		// Force-stop main spinner if it was active (state is now cleared)
		if m.spinnerActive {
			m.spinnerActive = false
			m.spinner.Stop()
		}
		// Stop and clear any in-flight RAG indexing spinners
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

		// Recreate the subagent row spinner (its frames embed theme styling).
		if m.subagentSpinnerOn {
			m.subagentSpinner.Stop()
			m.subagentSpinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
			cmds = append(cmds, m.subagentSpinner.Init())
		} else {
			m.subagentSpinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
		}

		m.todoComp.InvalidateCache() // Cached todo lines embed theme styling
		m.invalidateCache()          // Theme affects all styling
		return m, tea.Batch(cmds...)
	default:
		var cmds []tea.Cmd
		needsInvalidate := false

		// Update main spinner when tools are loading, agent is working, or title is regenerating
		if m.toolsLoading || m.workingAgent != "" || m.titleRegenerating {
			model, cmd := m.spinner.Update(msg)
			m.spinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			needsInvalidate = true
		}

		// Update each RAG indexing spinner
		for _, state := range m.ragIndexing {
			model, cmd := state.spinner.Update(msg)
			state.spinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			needsInvalidate = true
		}

		// Update the shared subagent row spinner while any subagent runs.
		if m.subagentSpinnerOn {
			model, cmd := m.subagentSpinner.Update(msg)
			m.subagentSpinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			needsInvalidate = true
		}

		// Invalidate cache when spinners update to show new animation frames.
		// This is animation-only (fixed-width glyph swap), so the cheaper path is used.
		if needsInvalidate {
			m.invalidateAnimation()
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
		WorkingIndicator: m.workingIndicatorCollapsed(),
		WorkingDir:       m.workingDirLine(),
		InfoLine:         m.collapsedInfoLine(),
		ContentWidth:     contentWidth,
	}
	if !m.sectionVisibility.HideUsage {
		vm.UsageSummary = m.tokenUsageSummary()
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

// collapsedInfoLine returns a one-line summary of the agents, tools, and
// todos sections for the collapsed band, honoring section visibility.
func (m *model) collapsedInfoLine() string {
	var parts []string
	appendPart := func(part string) {
		if part != "" {
			parts = append(parts, part)
		}
	}

	if !m.sectionVisibility.HideAgents {
		appendPart(m.agentSummaryCollapsed())
	}
	if !m.sectionVisibility.HideTools {
		appendPart(m.toolsSummaryCollapsed())
	}
	if !m.sectionVisibility.HideTodos {
		appendPart(m.todosSummaryCollapsed())
	}

	return strings.Join(parts, styles.MutedStyle.Render(" · "))
}

// agentSummaryCollapsed renders the current agent (in its accent color) with
// its model and the number of other agents in the team.
func (m *model) agentSummaryCollapsed() string {
	name := m.sessionState.CurrentAgentName()
	if name == "" {
		return ""
	}

	summary := styles.AgentAccentStyleFor(name).Render("▶ " + name)
	if m.agentModel != "" {
		summary += styles.MutedStyle.Render(" " + m.agentModel)
	}
	if n := len(m.availableAgents); n > 1 {
		summary += styles.MutedStyle.Render(fmt.Sprintf(" +%d", n-1))
	}
	return summary
}

// toolsSummaryCollapsed renders the tools/skills counts with the sidebar's
// accent block, or the loading spinner while the toolset is starting.
func (m *model) toolsSummaryCollapsed() string {
	if m.toolsLoading {
		return m.spinner.View() + styles.TabPrimaryStyle.Render(" loading tools…")
	}

	var parts []string
	if m.availableTools > 0 {
		label := "tools"
		if m.availableTools == 1 {
			label = "tool"
		}
		parts = append(parts, fmt.Sprintf("%d %s", m.availableTools, label))
	}
	if m.availableSkills > 0 {
		label := "skills"
		if m.availableSkills == 1 {
			label = "skill"
		}
		parts = append(parts, fmt.Sprintf("%d %s", m.availableSkills, label))
	}
	if len(parts) == 0 {
		return ""
	}
	return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(" "+strings.Join(parts, ", "))
}

// todosSummaryCollapsed renders the completed/total todo counts with the
// same status icons as the vertical TO-DO tab.
func (m *model) todosSummaryCollapsed() string {
	completed, total := m.todoComp.Counts()
	if total == 0 {
		return ""
	}

	var icon string
	switch completed {
	case total:
		icon = styles.CompletedStyle.Render("✓")
	case 0:
		icon = styles.ToBeDoneStyle.Render("◯")
	default:
		icon = styles.InProgressStyle.Render("◔")
	}
	return icon + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d/%d todos", completed, total))
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

	// Animation-only fast path: when only a spinner frame changed, the line count
	// and scrollbar visibility are unchanged, so reuse the cached scrollbar
	// decision and render the sections a single time instead of the two-pass probe.
	if !m.layoutDirty && len(m.cachedLines) > 0 && m.cachedWidth == contentWidthNoScroll {
		width := contentWidthNoScroll
		if m.cachedNeedsScrollbar {
			width = m.contentWidth(true)
		}
		m.cachedLines = m.renderSections(width)
		m.cacheDirty = false
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
	m.layoutDirty = false

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
// Sections are separated by sectionGap blank lines; each appendSection call
// returns the index of the section's first line so click zones can be anchored.
// On animation-only frames (layout clean, sectionCache warm) it re-renders
// just the sections whose content can change frame-to-frame — the ticking
// spinners and the hoverable subagent rows — and serves the rest from
// sectionCache, so an idle parent watching a working subagent costs one small
// section per frame instead of the whole sidebar.
func (m *model) renderSections(contentWidth int) []string {
	if m.sectionCache == nil || m.sectionCacheWidth != contentWidth {
		m.sectionCache = map[string]string{}
		m.sectionCacheWidth = contentWidth
	}

	var lines []string

	appendSection := func(section string) int {
		if section == "" {
			return len(lines)
		}
		if len(lines) > 0 {
			for range m.sectionGap {
				lines = append(lines, "")
			}
		}
		start := len(lines)
		lines = append(lines, strings.Split(section, "\n")...)
		return start
	}
	// cached returns the section's previous render unless it is animated (its
	// content can change on an animation-only frame) or not cached yet.
	cached := func(name string, animated bool, render func(int) string) string {
		if !animated {
			if s, ok := m.sectionCache[name]; ok {
				return s
			}
		}
		s := render(contentWidth)
		m.sectionCache[name] = s
		return s
	}

	mainSpinner := m.needsSpinner()
	appendSection(cached("session", m.titleRegenerating, m.sessionInfo))
	if !m.sectionVisibility.HideUsage {
		appendSection(cached("usage", false, m.tokenUsage))
	}
	appendSection(cached("queue", mainSpinner && len(m.queuedMessages) > 0, m.queueSection))

	// Track where subagent rows start so mouse motion can resolve hover.
	// Always re-rendered on animation frames: the row spinner ticks and hover
	// moves between rows, and rendering also refreshes the side state
	// (subagentLineNodes, subagentRowOffset) the hover zones are built from.
	subagentSectionStart := appendSection(cached("subagents", true, m.subagentsInfo))
	m.buildSubagentHoverZones(subagentSectionStart)

	// Track where agent entries start so we can detect clicks on agent names
	agentSectionStart := len(lines)
	if m.sectionVisibility.HideAgents {
		m.agentLineOwners = nil
	} else {
		agentSectionStart = appendSection(cached("agents", mainSpinner, m.agentInfo))
	}
	m.buildAgentClickZones(agentSectionStart)

	if !m.sectionVisibility.HideTools {
		appendSection(cached("toolsets", mainSpinner || len(m.ragIndexing) > 0, m.toolsetInfo))
	}

	if !m.sectionVisibility.HideTodos {
		appendSection(cached("todos", false, func(w int) string {
			m.todoComp.SetSize(w)
			return strings.TrimSuffix(m.todoComp.Render(), "\n")
		}))
	}

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
	contextLevel styles.ContextGaugeLevel
	totalCost    float64
	sessionCount int
}

func (m *model) computeUsageStats() usageStats {
	var s usageStats
	for _, usage := range m.sessionUsage {
		s.totalCost += usage.Cost
		s.sessionCount++
	}
	s.tokens, _ = m.activeSessionTokens()
	s.contextPct = m.contextPercent()
	if usage, ok := m.activeSessionUsage(); ok {
		s.contextLevel = usageGaugeLevel(usage)
	}
	return s
}

func (m *model) tokenUsage(contentWidth int) string {
	return m.renderTab("Token Usage", m.tokenUsageLine(), contentWidth)
}

// tokenUsageLine renders the usage line shared by the vertical Token Usage
// tab and the collapsed band: token glyph, count, context %, cost, and the
// sub-session count, all with the same styling. The context reading warns as
// it nears the compaction threshold and reads "compacting…" while a
// compaction runs.
func (m *model) tokenUsageLine() string {
	s := m.computeUsageStats()

	line := styles.MutedStyle.Render(styles.TokenGlyph+" ") + toolcommon.FormatTokenCount(s.tokens)
	switch {
	case m.compacting:
		line += " " + styles.WarningStyle.Render("(compacting…)")
	case s.contextPct != "":
		line += " " + contextGaugeStyle(s.contextLevel, styles.NoStyle).Render("("+s.contextPct+")")
	}
	line += " " + styles.TabAccentStyle.Render(toolcommon.FormatCostUSD(s.totalCost))
	if s.sessionCount > 1 {
		line += " " + styles.MutedStyle.Render(fmt.Sprintf("(%d sub-sessions)", s.sessionCount-1))
	}

	return line
}

// tokenUsageSummary returns the usage line for the collapsed band, empty
// until any usage has been recorded.
func (m *model) tokenUsageSummary() string {
	if len(m.sessionUsage) == 0 {
		return ""
	}
	return m.tokenUsageLine()
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

	lines := []string{
		titleLine,
		"",
	}

	if m.workingDirectory != "" {
		lines = append(lines, m.workingDirLine())
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

// parentLine renders the attached tab's clickable "parent: <agent>" line,
// with the agent name accent-colored (brightened while hovered). Rendered as
// the first row of the Subagents section.
func (m *model) parentLine() string {
	nameStyle := styles.AgentAccentStyleFor(m.parentAgent)
	if m.hoveredParent {
		if fg := nameStyle.GetForeground(); fg != nil {
			nameStyle = nameStyle.Foreground(styles.Brighten(fg, subagentHoverBrighten))
		}
	}
	return styles.MutedStyle.Render("parent: ") + nameStyle.Render(m.parentAgent)
}

// subagentsInfo renders the live subagent swarm: an animated spinner (or
// terminal-state glyph), the agent-colored name, and a right-aligned status
// column showing the node state — or, for the hovered row, the node id beside
// the name and when it was spawned. On an attached tab the section opens with a
// clickable "parent: <agent>" line, separated from the children tree by a blank
// line. It records per-row node ownership (and the parent-line offset) so
// renderSections can register hover/click zones.
func (m *model) subagentsInfo(contentWidth int) string {
	m.subagentLineNodes = nil
	m.subagentRowOffset = 0
	if len(m.subagentNodes) == 0 && m.parentAgent == "" {
		return ""
	}
	var lines []string
	if m.parentAgent != "" {
		lines = append(lines, m.parentLine())
		if len(m.subagentNodes) > 0 {
			lines = append(lines, "")
		}
		m.subagentRowOffset = len(lines)
	}
	var walk func(nodes []subagent.NodeSnapshot, prefix string)
	walk = func(nodes []subagent.NodeSnapshot, prefix string) {
		for i := range nodes {
			last := i == len(nodes)-1
			connector, childPrefix := prefix+"├", prefix+"│"
			if last {
				connector, childPrefix = prefix+"└", prefix+" "
			}
			lines = append(lines, m.subagentLine(nodes[i].Node, connector, contentWidth))
			m.subagentLineNodes = append(m.subagentLineNodes, nodes[i].Node.ID)
			walk(nodes[i].Children, childPrefix)
		}
	}
	// Top-level subagents render bare; branch guides start at their children.
	for i := range m.subagentNodes {
		lines = append(lines, m.subagentLine(m.subagentNodes[i].Node, "", contentWidth))
		m.subagentLineNodes = append(m.subagentLineNodes, m.subagentNodes[i].Node.ID)
		walk(m.subagentNodes[i].Children, "")
	}
	return m.renderTab("Subagents", strings.Join(lines, "\n"), contentWidth)
}

// subagentHoverBrighten is the HSL lightness bump applied to a hovered
// subagent name's accent color.
const subagentHoverBrighten = 0.25

// subagentLine renders one swarm row: compact muted branch guides, a state
// glyph beside the agent name, and a muted status (or spawn time when hovered)
// right-aligned. The name is always kept on the first line; it truncates with
// an ellipsis before the row can wrap.
func (m *model) subagentLine(n subagent.Node, guides string, contentWidth int) string {
	hovered := m.hoveredSubagent == n.ID

	nameStyle := styles.AgentAccentStyleFor(n.Agent)
	if hovered {
		if fg := nameStyle.GetForeground(); fg != nil {
			nameStyle = nameStyle.Foreground(styles.Brighten(fg, subagentHoverBrighten))
		}
	}

	rightText := string(n.State)
	if hovered {
		rightText = timeAgo(n.CreatedAt)
	}
	right := styles.MutedStyle.Render(rightText)
	rightWidth := lipgloss.Width(right)

	leftBudget := contentWidth - rightWidth - 1
	if leftBudget < 2 {
		right = ""
		rightWidth = 0
		leftBudget = contentWidth
	}

	glyph := m.subagentGlyph(n)
	maxGuideWidth := max(0, leftBudget-lipgloss.Width(glyph)-2)
	guides = subagentGuideTail(guides, maxGuideWidth)
	prefix := styles.MutedStyle.Render(guides) + glyph
	separator := " "
	nameWidth := leftBudget - lipgloss.Width(prefix) - lipgloss.Width(separator)
	if nameWidth <= 0 {
		separator = ""
		nameWidth = max(1, leftBudget-lipgloss.Width(prefix))
	}

	styledName := m.subagentName(n, nameStyle, nameWidth, hovered)
	left := prefix + separator + styledName

	if right == "" {
		return left
	}
	gap := max(1, contentWidth-lipgloss.Width(left)-rightWidth)
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) subagentName(n subagent.Node, nameStyle lipgloss.Style, width int, hovered bool) string {
	name := n.DisplayName()
	if width <= 0 {
		return ""
	}
	if !hovered {
		return nameStyle.Render(toolcommon.TruncateText(name, width))
	}

	suffix := fmt.Sprintf(" (%s)", n.ID)
	if width > lipgloss.Width(suffix)+1 {
		nameWidth := width - lipgloss.Width(suffix)
		return nameStyle.Render(toolcommon.TruncateText(name, nameWidth)) + styles.MutedStyle.Render(suffix)
	}
	return nameStyle.Render(toolcommon.TruncateText(name, width))
}

func subagentGuideTail(guides string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(guides) <= width {
		return guides
	}
	runes := []rune(guides)
	start := len(runes)
	used := 0
	for start > 0 {
		w := lipgloss.Width(string(runes[start-1]))
		if used+w > width {
			break
		}
		start--
		used += w
	}
	return string(runes[start:])
}

// subagentGlyph picks the inline glyph for a swarm row: an animated spinner
// while the subagent works, ○ while it idles, ✓/✗ for terminal states. Every
// glyph is a single text cell so state transitions never shift the row's text.
func (m *model) subagentGlyph(n subagent.Node) string {
	switch {
	case isActiveSubagentState(n.State):
		return styles.AgentAccentStyleFor(n.Agent).Render(m.subagentSpinner.RawFrame())
	case n.State == subagent.NodeFailed:
		return styles.ErrorStyle.Render("✗")
	case n.State == subagent.NodeIdle:
		return styles.MutedStyle.Render("○")
	default:
		return styles.SuccessStyle.Render("✓")
	}
}

// timeAgo formats how long ago t happened, compactly ("just now", "5m ago").
func timeAgo(t time.Time) string {
	elapsed := time.Since(t)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
}

// buildSubagentHoverZones maps rendered subagent rows to content lines, using
// the same fixed renderTab header offset as the agent click zones. The
// attached tab's "parent: <agent>" line (row 0 of the section, when present)
// gets its own zone; children rows follow after subagentRowOffset.
func (m *model) buildSubagentHoverZones(sectionStart int) {
	m.subagentHoverZone = make(map[int]subagent.NodeID, len(m.subagentLineNodes))
	const tabHeaderLines = 2 // tab title + TabStyle top padding
	m.parentLineZone = -1
	if m.parentAgent != "" {
		m.parentLineZone = sectionStart + tabHeaderLines
	}
	for j, id := range m.subagentLineNodes {
		m.subagentHoverZone[sectionStart+tabHeaderLines+m.subagentRowOffset+j] = id
	}
}

// updateSubagentHover resolves the subagent row under the cursor (sidebar-
// relative y) and re-renders when the hovered row changes.
func (m *model) updateSubagentHover(y int) {
	contentY := y + m.scrollview.ScrollOffset()
	id := m.subagentHoverZone[contentY]
	if id != m.hoveredSubagent {
		m.hoveredSubagent = id
		m.invalidateAnimation()
	}
	hoveredParent := m.parentLineZone >= 0 && contentY == m.parentLineZone
	if hoveredParent != m.hoveredParent {
		m.hoveredParent = hoveredParent
		m.invalidateAnimation()
	}
}

// ClearSubagentHover resets subagent hover state, e.g. when the mouse leaves
// the sidebar.
func (m *model) ClearSubagentHover() {
	if m.hoveredSubagent != "" {
		m.hoveredSubagent = ""
		m.invalidateAnimation()
	}
	if m.hoveredParent {
		m.hoveredParent = false
		m.invalidateAnimation()
	}
}

// agentInfo renders the Agents panel: every agent as a two-line entry, with a
// blank separator line between entries. Line 1 shows the agent's name (in its
// accent color), the thinking badge right-aligned, and the "^N" switch shortcut
// flush against the right edge; line 2 shows the indented provider/model with
// the agent's latest context usage percentage right-aligned once known. The
// current agent is marked with ▶ (or the spinner while it works); the other
// agents pad that marker column so their names stay aligned. Descriptions are
// deliberately omitted. Each content line is owned by its agent (agentLineOwners)
// so click zones can be registered explicitly (see buildAgentClickZones) and a
// click on either line switches to that agent; separators carry an empty owner
// so they stay unclickable.
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

	// Compute the shared column widths once so every entry aligns and the badge
	// width is not recomputed per agent.
	glyphOnly := contentWidth < rowGlyphOnlyMinWidth
	badgeWidth := m.badgeColumnWidth(glyphOnly)
	nameWidth := max(1, contentWidth-agentMarkerWidth-minGap-badgeWidth-1-agentShortcutWidth)

	var bodyLines, owners []string
	add := func(line, owner string) {
		bodyLines = append(bodyLines, line)
		owners = append(owners, owner)
	}
	for i, agent := range m.availableAgents {
		// Separate entries with a blank, unowned line so the two-line entries stay
		// visually distinct without being attributed to (or made clickable for)
		// any agent.
		if len(bodyLines) > 0 {
			add("", "")
		}
		current := agent.Name == currentAgent
		for _, line := range m.renderAgentLine(agent, i, contentWidth, nameWidth, badgeWidth, glyphOnly, current) {
			add(line, agent.Name)
		}
	}
	m.agentLineOwners = owners

	return m.renderTab(agentTitle, strings.Join(bodyLines, "\n"), contentWidth)
}

// thinkingKind classifies an agent's raw thinking wire label into the badge
// vocabulary used by the agent lines.
type thinkingKind int

const (
	thinkingNone     thinkingKind = iota // empty label: no badge
	thinkingOff                          // "off": disabled on a capable model
	thinkingAdaptive                     // "adaptive": adaptive budget
	thinkingTokens                       // decimal token count
	thinkingLevel                        // effort level word (e.g. "high")
)

// classifyThinking maps a raw wire label to its kind. For token budgets it also
// returns the parsed token count.
func classifyThinking(label string) (thinkingKind, int64) {
	switch label {
	case "":
		return thinkingNone, 0
	case "off":
		return thinkingOff, 0
	case "adaptive":
		return thinkingAdaptive, 0
	}
	if isAllDigits(label) {
		n, _ := strconv.ParseInt(label, 10, 64)
		return thinkingTokens, n
	}
	return thinkingLevel, 0
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// thinkingBadge returns the styled right-aligned badge for an agent's thinking
// label and the compact single-cell form used in the glyph-only degradation
// step. Both are empty when the agent has no thinking configuration. The
// vocabulary carries no ✻ glyph: the effort gauge is the only visual language
// for thinking.
func thinkingBadge(label string) (badge, compact string) {
	kind, tokens := classifyThinking(label)
	switch kind {
	case thinkingNone:
		return "", ""
	case thinkingOff:
		// Capable but disabled: a dim empty gauge, distinct from a non-capable
		// model (which renders nothing). The compact form is a single empty cell.
		cell := lipgloss.NewStyle().Foreground(styles.TextMuted).Faint(true).Render(styles.GaugeEmpty)
		return toolcommon.EffortGaugeEmpty(), cell
	case thinkingAdaptive:
		auto := styles.ThinkingBadgeStyle.Render("auto")
		return auto, auto
	case thinkingTokens:
		return styles.ThinkingBadgeStyle.Render(styles.TokenGlyph + " " + toolcommon.FormatTokenCount(tokens)),
			styles.ThinkingBadgeStyle.Render(styles.TokenGlyph)
	default: // thinkingLevel
		level, ok := effort.Parse(label)
		if !ok {
			// Unknown/future level word: a plain text badge so it still renders.
			b := styles.ThinkingBadgeStyle.Render(label)
			return b, b
		}
		return toolcommon.EffortGauge(level), toolcommon.EffortFillStyle(level).Render(styles.GaugeFilled)
	}
}

// badgeColumnWidth returns the widest thinking badge across the roster so every
// agent line reserves the same badge column and the badges stay aligned.
// glyphOnly selects the single-cell compact form used near MinWidth.
func (m *model) badgeColumnWidth(glyphOnly bool) int {
	w := 0
	for _, a := range m.availableAgents {
		full, compact := thinkingBadge(a.Thinking)
		b := full
		if glyphOnly {
			b = compact
		}
		w = max(w, lipgloss.Width(b))
	}
	return w
}

// padRight pads a (possibly styled) string with trailing spaces to width.
func padRight(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

// padLeft pads a (possibly styled) string with leading spaces to width, i.e.
// right-aligns it within the column.
func padLeft(s string, width int) string {
	return strings.Repeat(" ", max(0, width-lipgloss.Width(s))) + s
}

// renderAgentLine renders a single agent as two lines:
//
//	▶ name            <thinking> ^N
//	  provider/model         <ctx%>
//
// Line 1: the name is left-aligned in its accent color, the thinking badge is
// right-aligned in a shared column, and the "^N" switch shortcut sits flush
// against the right edge. The current agent is marked with ▶ (or the spinner
// while it — or any agent — is working); other agents pad the marker column so
// the names stay aligned. Line 2: the indented provider/model, left-truncated
// so its informative tail survives, with the agent's latest known context
// usage percentage right-aligned once the agent has run (background agent
// tasks included). The description is omitted. Agents past the 9th have no
// shortcut.
func (m *model) renderAgentLine(agent runtime.AgentDetails, index, contentWidth, nameWidth, badgeWidth int, glyphOnly, current bool) []string {
	agentStyle := styles.AgentAccentStyleFor(agent.Name)

	var marker string
	switch {
	case m.workingAgent == agent.Name:
		marker = agentStyle.Render(m.spinner.RawFrame())
	case current:
		marker = agentStyle.Render("▶")
	}
	left := padRight(marker, agentMarkerWidth) + agentStyle.Render(toolcommon.TruncateText(agent.Name, nameWidth))

	badge, compact := thinkingBadge(agent.Thinking)
	if glyphOnly {
		badge = compact
	}

	var shortcut string
	if index >= 0 && index < 9 {
		shortcut = styles.MutedStyle.Render(fmt.Sprintf("^%d", index+1))
	}

	right := padLeft(badge, badgeWidth) + " " + padLeft(shortcut, agentShortcutWidth)
	gap := max(1, contentWidth-lipgloss.Width(left)-lipgloss.Width(right))
	line1 := left + strings.Repeat(" ", gap) + right

	modelText := agent.Model
	if agent.Provider != "" {
		modelText = agent.Provider + "/" + agent.Model
	}
	ctxPercent := m.agentContextPercent(agent.Name)
	ctxStyle := contextGaugeStyle(m.agentContextGaugeLevel(agent.Name), styles.MutedStyle)
	modelWidth := contentWidth - agentMarkerWidth
	if ctxPercent != "" {
		modelWidth -= lipgloss.Width(ctxPercent) + 1
	}
	model := toolcommon.TruncateTextLeft(modelText, max(1, modelWidth))
	line2 := strings.Repeat(" ", agentMarkerWidth) + styles.MutedStyle.Render(model)
	if ctxPercent != "" {
		gap := max(1, contentWidth-lipgloss.Width(line2)-lipgloss.Width(ctxPercent))
		line2 += strings.Repeat(" ", gap) + ctxStyle.Render(ctxPercent)
	}

	return []string{line1, line2}
}

// buildAgentClickZones populates agentClickZones from the explicit per-line
// ownership recorded by agentInfo while rendering. agentSectionStart is the
// index of the agent section's first rendered line; the renderTab wrapper adds
// a fixed 2-line header (tab title + TabStyle top padding) before the body, so
// body line j maps to content line agentSectionStart+tabHeaderLines+j. Lines
// with no owner (blank separators) are not registered.
func (m *model) buildAgentClickZones(agentSectionStart int) {
	m.agentClickZones = make(map[int]string)

	const tabHeaderLines = 2 // tab title + TabStyle top padding
	for j, owner := range m.agentLineOwners {
		if owner == "" {
			continue
		}
		m.agentClickZones[agentSectionStart+tabHeaderLines+j] = owner
	}
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
	m.mode = mode
	m.invalidateCache()
}

// SetMirroredPadding swaps the horizontal edge padding so the content hugs
// the terminal edge when the sidebar is rendered on the left of the chat,
// with the breathing room moved to the chat side.
func (m *model) SetMirroredPadding(mirrored bool) {
	cfg := DefaultLayoutConfig()
	if mirrored {
		cfg.PaddingLeft, cfg.PaddingRight = cfg.PaddingRight, cfg.PaddingLeft
	}
	if m.layoutCfg == cfg {
		return
	}
	m.layoutCfg = cfg
	m.updateScrollviewPosition()
	m.updateTitleInputWidth()
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
