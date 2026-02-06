// Package background provides background agents feature support.
package background

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/cagent/pkg/app"
	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/tui/animation"
	"github.com/docker/cagent/pkg/tui/commands"
	"github.com/docker/cagent/pkg/tui/components/completion"
	"github.com/docker/cagent/pkg/tui/components/notification"
	"github.com/docker/cagent/pkg/tui/components/statusbar"
	"github.com/docker/cagent/pkg/tui/components/tabbar"
	"github.com/docker/cagent/pkg/tui/components/tool/editfile"
	"github.com/docker/cagent/pkg/tui/core"
	"github.com/docker/cagent/pkg/tui/dialog"
	"github.com/docker/cagent/pkg/tui/messages"
	"github.com/docker/cagent/pkg/tui/page/chat"
	"github.com/docker/cagent/pkg/tui/service"
	"github.com/docker/cagent/pkg/tui/service/supervisor"
	"github.com/docker/cagent/pkg/tui/service/tabstore"
	"github.com/docker/cagent/pkg/tui/styles"
)

// SessionSpawner creates new sessions with their own runtime.
// This is an alias to the supervisor package's SessionSpawner type.
type SessionSpawner = supervisor.SessionSpawner

// Model is the multi-session TUI model that wraps the single-session chat page.
type Model struct {
	supervisor *supervisor.Supervisor
	tabBar     *tabbar.TabBar
	tabStore   *tabstore.Store

	// Per-session chat pages (kept alive for streaming continuity)
	chatPages     map[string]chat.Page
	sessionStates map[string]*service.SessionState

	// Active session (convenience pointers to the currently visible session)
	application  *app.App
	sessionState *service.SessionState
	chatPage     chat.Page

	// UI components
	notification notification.Manager
	dialogMgr    dialog.Manager
	statusBar    statusbar.StatusBar
	completions  completion.Manager

	// Window state
	wWidth, wHeight int
	width, height   int

	// keyboardEnhancements stores the last keyboard enhancements message
	keyboardEnhancements *tea.KeyboardEnhancementsMsg

	ready bool
	err   error
}

// New creates a new multi-session model.
func New(ctx context.Context, spawner SessionSpawner, initialApp *app.App, initialWorkingDir string, cleanup func()) tea.Model {
	// Initialize supervisor
	sv := supervisor.New(spawner)

	// Initialize tab bar
	tb := tabbar.New()

	// Initialize tab store
	var ts *tabstore.Store
	var tsErr error
	ts, tsErr = tabstore.New()
	if tsErr != nil {
		slog.Warn("Failed to open tab store, tabs won't persist", "error", tsErr)
	}

	initialSessionState := service.NewSessionState(initialApp.Session())
	initialChatPage := chat.New(initialApp, initialSessionState)
	sessID := initialApp.Session().ID

	m := &Model{
		supervisor:    sv,
		tabBar:        tb,
		tabStore:      ts,
		chatPages:     map[string]chat.Page{sessID: initialChatPage},
		sessionStates: map[string]*service.SessionState{sessID: initialSessionState},
		application:   initialApp,
		sessionState:  initialSessionState,
		chatPage:      initialChatPage,
		notification:  notification.New(),
		dialogMgr:     dialog.New(),
		completions:   completion.New(),
	}

	// Initialize status bar (pass m as help provider)
	m.statusBar = statusbar.New(m)

	// Add the initial session to the supervisor
	sv.AddSession(ctx, initialApp, initialApp.Session(), initialWorkingDir, cleanup)

	// Persist initial tab
	if ts != nil {
		sess := initialApp.Session()
		_ = ts.AddTab(ctx, sess.ID, initialWorkingDir)
		_ = ts.SetActiveTab(ctx, sess.ID)
	}

	// Initialize tab bar with current tabs
	tabs, activeIdx := sv.GetTabs()
	tb.SetTabs(tabs, activeIdx)

	// Make sure to stop on context cancellation
	go func() {
		<-ctx.Done()
		for _, cp := range m.chatPages {
			cp.Cleanup()
		}
		sv.Shutdown()
		if ts != nil {
			_ = ts.ClearTabs(context.Background())
			_ = ts.Close()
		}
	}()

	return m
}

// SetProgram sets the tea.Program for the supervisor to send routed messages.
func (m *Model) SetProgram(p *tea.Program) {
	m.supervisor.SetProgram(p)
}

// Init initializes the model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.dialogMgr.Init(),
		m.chatPage.Init(),
		m.application.SendFirstMessage(),
	)
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	// --- Routing & Animation ---

	case messages.RoutedMsg:
		return m.handleRoutedMsg(msg)

	case animation.TickMsg:
		var cmds []tea.Cmd
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		cmds = append(cmds, cmd)
		if animation.HasActive() {
			cmds = append(cmds, animation.StartTick())
		}
		return m, tea.Batch(cmds...)

	// --- Tab management ---

	case messages.TabsUpdatedMsg:
		m.tabBar.SetTabs(msg.Tabs, msg.ActiveIdx)
		return m, nil

	case messages.SpawnSessionMsg:
		return m.handleSpawnSession(msg.WorkingDir)

	case messages.SwitchTabMsg:
		return m.handleSwitchTab(msg.SessionID)

	case messages.CloseTabMsg:
		return m.handleCloseTab(msg.SessionID)

	// --- Window / Terminal ---

	case tea.WindowSizeMsg:
		m.wWidth, m.wHeight = msg.Width, msg.Height
		cmd := m.handleWindowResize(msg.Width, msg.Height)
		return m, cmd

	case tea.KeyboardEnhancementsMsg:
		m.keyboardEnhancements = &msg
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		return m, cmd

	// --- Keyboard input ---

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)

	case tea.PasteMsg:
		if m.dialogMgr.Open() {
			u, cmd := m.dialogMgr.Update(msg)
			m.dialogMgr = u.(dialog.Manager)
			return m, cmd
		}
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		return m, cmd

	// --- Mouse ---

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)

	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)

	case messages.WheelCoalescedMsg:
		return m.handleWheelCoalesced(msg)

	// --- Dialog lifecycle ---

	case dialog.OpenDialogMsg, dialog.CloseDialogMsg:
		u, cmd := m.dialogMgr.Update(msg)
		m.dialogMgr = u.(dialog.Manager)
		return m, cmd

	case dialog.ExitConfirmedMsg:
		for _, cp := range m.chatPages {
			cp.Cleanup()
		}
		return m, tea.Quit

	case dialog.RuntimeResumeMsg:
		m.application.Resume(msg.Request)
		return m, nil

	case dialog.MultiChoiceResultMsg:
		if msg.DialogID == dialog.ToolRejectionDialogID {
			if msg.Result.IsCancelled {
				return m, nil
			}
			resumeMsg := dialog.HandleToolRejectionResult(msg.Result)
			if resumeMsg != nil {
				return m, tea.Sequence(
					core.CmdHandler(dialog.CloseDialogMsg{}),
					core.CmdHandler(*resumeMsg),
				)
			}
		}
		return m, nil

	// --- Notifications ---

	case notification.ShowMsg, notification.HideMsg:
		updated, cmd := m.notification.Update(msg)
		m.notification = updated
		return m, cmd

	// --- Runtime event specializations ---

	case *runtime.TeamInfoEvent:
		m.sessionState.SetAvailableAgents(msg.AvailableAgents)
		m.sessionState.SetCurrentAgentName(msg.CurrentAgent)
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		return m, cmd

	case *runtime.AgentInfoEvent:
		m.sessionState.SetCurrentAgentName(msg.AgentName)
		m.application.TrackCurrentAgentModel(msg.Model)
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		return m, cmd

	case *runtime.SessionTitleEvent:
		m.sessionState.SetSessionTitle(msg.Title)
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		return m, cmd

	// --- New session (slash command /new) ---

	case messages.NewSessionMsg:
		// In background mode, /new spawns a new tab.
		return m.handleSpawnSession("")

	// --- Exit ---

	case messages.ExitSessionMsg:
		for _, cp := range m.chatPages {
			cp.Cleanup()
		}
		return m, tea.Quit

	case messages.ExitAfterFirstResponseMsg:
		for _, cp := range m.chatPages {
			cp.Cleanup()
		}
		return m, tea.Quit

	// --- Agent management ---

	case messages.SwitchAgentMsg:
		return m.handleSwitchAgent(msg.AgentName)

	// --- Session browser ---

	case messages.OpenSessionBrowserMsg:
		return m.handleOpenSessionBrowser()

	case messages.LoadSessionMsg:
		return m.handleLoadSession(msg.SessionID)

	// --- Session commands (slash commands, command palette) ---

	case messages.ToggleYoloMsg:
		return m.handleToggleYolo()

	case messages.ToggleThinkingMsg:
		return m.handleToggleThinking()

	case messages.ToggleHideToolResultsMsg:
		return m.handleToggleHideToolResults()

	case messages.ToggleSplitDiffMsg:
		updated, cmd := m.chatPage.Update(editfile.ToggleDiffViewMsg{})
		m.chatPage = updated.(chat.Page)
		return m, cmd

	case messages.ClearQueueMsg:
		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		return m, cmd

	case messages.CompactSessionMsg:
		return m.handleCompactSession(msg.AdditionalPrompt)

	case messages.CopySessionToClipboardMsg:
		return m.handleCopySessionToClipboard()

	case messages.CopyLastResponseToClipboardMsg:
		return m.handleCopyLastResponseToClipboard()

	case messages.EvalSessionMsg:
		return m.handleEvalSession(msg.Filename)

	case messages.ExportSessionMsg:
		return m.handleExportSession(msg.Filename)

	case messages.ToggleSessionStarMsg:
		sessionID := msg.SessionID
		if sessionID == "" {
			if sess := m.application.Session(); sess != nil {
				sessionID = sess.ID
			} else {
				return m, nil
			}
		}
		return m.handleToggleSessionStar(sessionID)

	case messages.SetSessionTitleMsg:
		return m.handleSetSessionTitle(msg.Title)

	case messages.RegenerateTitleMsg:
		return m.handleRegenerateTitle()

	case messages.ShowCostDialogMsg:
		return m.handleShowCostDialog()

	case messages.ShowPermissionsDialogMsg:
		return m.handleShowPermissionsDialog()

	case messages.AgentCommandMsg:
		return m.handleAgentCommand(msg.Command)

	case messages.StartShellMsg:
		return m.startShell()

	// --- Model picker ---

	case messages.OpenModelPickerMsg:
		return m.handleOpenModelPicker()

	case messages.ChangeModelMsg:
		return m.handleChangeModel(msg.ModelRef)

	// --- Theme picker ---

	case messages.OpenThemePickerMsg:
		return m.handleOpenThemePicker()

	case messages.ChangeThemeMsg:
		return m.handleChangeTheme(msg.ThemeRef)

	case messages.ThemePreviewMsg:
		return m.handleThemePreview(msg.ThemeRef)

	case messages.ThemeCancelPreviewMsg:
		return m.handleThemeCancelPreview(msg.OriginalRef)

	case messages.ThemeChangedMsg:
		return m.applyThemeChanged()

	case messages.ThemeFileChangedMsg:
		return m.handleThemeFileChanged(msg.ThemeRef)

	// --- Speech-to-text ---

	case messages.StartSpeakMsg:
		return m, notification.InfoCmd("Speech-to-text is not yet supported in multi-tab mode")

	case messages.StopSpeakMsg, messages.SpeakTranscriptMsg:
		return m, nil

	// --- MCP prompts ---

	case messages.ShowMCPPromptInputMsg:
		return m.handleShowMCPPromptInput(msg.PromptName, msg.PromptInfo)

	case messages.MCPPromptMsg:
		return m.handleMCPPrompt(msg.PromptName, msg.Arguments)

	// --- File attachments ---

	case messages.AttachFileMsg:
		return m.handleAttachFile(msg.FilePath)

	case messages.SendAttachmentMsg:
		m.application.RunWithMessage(context.Background(), nil, msg.Content)
		return m, nil

	// --- URL opening ---

	case messages.OpenURLMsg:
		return m.handleOpenURL(msg.URL)

	// --- Elicitation ---

	case messages.ElicitationResponseMsg:
		return m.handleElicitationResponse(msg.Action, msg.Content)

	// --- Editor height changed ---

	case chat.EditorHeightChangedMsg:
		m.completions.SetEditorBottom(msg.Height)
		return m, nil

	// --- Errors ---

	case error:
		m.err = msg
		return m, nil

	default:
		// Handle runtime events for active session
		if event, isRuntimeEvent := msg.(runtime.Event); isRuntimeEvent {
			if agentName := event.GetAgentName(); agentName != "" {
				m.sessionState.SetCurrentAgentName(agentName)
			}
			updated, cmd := m.chatPage.Update(msg)
			m.chatPage = updated.(chat.Page)
			return m, cmd
		}

		// Forward to dialog if open
		if m.dialogMgr.Open() {
			u, cmd := m.dialogMgr.Update(msg)
			m.dialogMgr = u.(dialog.Manager)

			updated, cmdChatPage := m.chatPage.Update(msg)
			m.chatPage = updated.(chat.Page)

			return m, tea.Batch(cmd, cmdChatPage)
		}

		// Forward to both completion manager and chat page
		updatedComp, cmdCompletions := m.completions.Update(msg)
		m.completions = updatedComp.(completion.Manager)

		updated, cmdChatPage := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)

		return m, tea.Batch(cmdCompletions, cmdChatPage)
	}
}

// handleRoutedMsg processes messages routed to specific sessions.
func (m *Model) handleRoutedMsg(msg messages.RoutedMsg) (tea.Model, tea.Cmd) {
	activeID := m.supervisor.ActiveID()

	if msg.SessionID == activeID {
		// Active session: forward through Update for full processing (spinners, cmds, etc.)
		return m.Update(msg.Inner)
	}

	// Background session: update its chat page directly so streaming content accumulates.
	// UI-only cmds (spinners, scroll) are discarded since the page isn't visible.
	chatPage, ok := m.chatPages[msg.SessionID]
	if !ok {
		return m, nil
	}

	// Update session state for background sessions
	if event, isRuntimeEvent := msg.Inner.(runtime.Event); isRuntimeEvent {
		if sessionState, ok := m.sessionStates[msg.SessionID]; ok {
			if agentName := event.GetAgentName(); agentName != "" {
				sessionState.SetCurrentAgentName(agentName)
			}
		}
	}

	// Update the background chat page (discard cmds — UI effects aren't needed for hidden pages)
	updated, _ := chatPage.Update(msg.Inner)
	m.chatPages[msg.SessionID] = updated.(chat.Page)
	return m, nil
}

// handleOpenSessionBrowser opens the session browser dialog.
func (m *Model) handleOpenSessionBrowser() (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.InfoCmd("No session store configured")
	}

	sessions, err := store.GetSessionSummaries(context.Background())
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load sessions: %v", err))
	}
	if len(sessions) == 0 {
		return m, notification.InfoCmd("No previous sessions found")
	}

	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSessionBrowserDialog(sessions),
	})
}

// handleLoadSession loads a saved session into a new in-focus tab.
func (m *Model) handleLoadSession(sessionID string) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}

	sess, err := store.GetSession(context.Background(), sessionID)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load session: %v", err))
	}

	slog.Debug("Loading session into new tab", "session_id", sessionID)

	// Determine working directory from the loaded session.
	workingDir := sess.WorkingDir
	if workingDir == "" {
		workingDir = m.application.Session().WorkingDir
	}

	// Spawn a new tab.
	ctx := context.Background()
	newSessionID, err := m.supervisor.SpawnSession(ctx, workingDir)
	if err != nil {
		return m, notification.ErrorCmd("Failed to create tab: " + err.Error())
	}

	if m.tabStore != nil {
		_ = m.tabStore.AddTab(ctx, newSessionID, workingDir)
	}

	// Switch to the new tab so m.application points to the new app.
	model, switchCmd := m.handleSwitchTab(newSessionID)

	// Replace the blank session with the loaded one and rebuild the chat page.
	m.application.ReplaceSession(ctx, sess)
	m.sessionState = service.NewSessionState(sess)
	m.chatPage = chat.New(m.application, m.sessionState)
	m.chatPages[newSessionID] = m.chatPage
	m.sessionStates[newSessionID] = m.sessionState

	if m.keyboardEnhancements != nil {
		updated, _ := m.chatPage.Update(*m.keyboardEnhancements)
		m.chatPage = updated.(chat.Page)
	}

	return model, tea.Batch(
		switchCmd,
		m.chatPage.Init(),
		m.chatPage.SetSize(m.width, m.height),
	)
}

// handleSpawnSession spawns a new session.
func (m *Model) handleSpawnSession(workingDir string) (tea.Model, tea.Cmd) {
	// If no working dir specified, open the picker
	if workingDir == "" {
		return m.openWorkingDirPicker()
	}

	// Spawn the new session
	ctx := context.Background()
	sessionID, err := m.supervisor.SpawnSession(ctx, workingDir)
	if err != nil {
		return m, notification.ErrorCmd("Failed to spawn session: " + err.Error())
	}

	// Persist the new tab
	if m.tabStore != nil {
		_ = m.tabStore.AddTab(ctx, sessionID, workingDir)
	}

	// Switch to the new session
	return m.handleSwitchTab(sessionID)
}

// openWorkingDirPicker opens the working directory picker dialog.
func (m *Model) openWorkingDirPicker() (tea.Model, tea.Cmd) {
	var recentDirs []string
	if m.tabStore != nil {
		recentDirs, _ = m.tabStore.GetRecentDirs(context.Background(), 10)
	}

	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewWorkingDirPickerDialog(recentDirs),
	})
}

// handleSwitchTab switches to a different session.
// Existing chat pages are preserved (not recreated) so that in-flight streaming
// content is retained when switching back to a tab.
func (m *Model) handleSwitchTab(sessionID string) (tea.Model, tea.Cmd) {
	runner := m.supervisor.SwitchTo(sessionID)
	if runner == nil {
		return m, notification.ErrorCmd("Session not found")
	}

	// Get or create session state
	sessionState, stateExists := m.sessionStates[sessionID]
	if !stateExists {
		sessionState = service.NewSessionState(runner.App.Session())
		m.sessionStates[sessionID] = sessionState
	}

	// Get or create chat page
	chatPage, pageExists := m.chatPages[sessionID]
	if !pageExists {
		chatPage = chat.New(runner.App, sessionState)
		m.chatPages[sessionID] = chatPage
	}

	// Update active convenience pointers
	m.application = runner.App
	m.sessionState = sessionState
	m.chatPage = chatPage

	// Reapply keyboard enhancements
	if m.keyboardEnhancements != nil {
		updated, _ := m.chatPage.Update(*m.keyboardEnhancements)
		m.chatPage = updated.(chat.Page)
	}

	// Persist active tab
	if m.tabStore != nil {
		_ = m.tabStore.SetActiveTab(context.Background(), sessionID)
	}

	if !pageExists {
		// New chat page: initialize and resize
		return m, tea.Batch(
			m.chatPage.Init(),
			m.chatPage.SetSize(m.width, m.height),
		)
	}

	// Existing chat page: resize and restore auto-scroll if it was active.
	// Background event processing discards scroll commands, so we must
	// explicitly re-engage auto-scroll when bringing a page back to front.
	return m, tea.Batch(
		m.chatPage.SetSize(m.width, m.height),
		m.chatPage.ScrollToBottom(),
	)
}

// handleCloseTab closes a session tab.
func (m *Model) handleCloseTab(sessionID string) (tea.Model, tea.Cmd) {
	wasActive := sessionID == m.supervisor.ActiveID()
	nextActiveID := m.supervisor.CloseSession(sessionID)

	// Clean up per-session state
	if cp, ok := m.chatPages[sessionID]; ok {
		cp.Cleanup()
		delete(m.chatPages, sessionID)
	}
	delete(m.sessionStates, sessionID)

	// Remove from persistent store
	if m.tabStore != nil {
		_ = m.tabStore.RemoveTab(context.Background(), sessionID)
	}

	// If we closed all tabs, spawn a new one in the default directory
	if m.supervisor.Count() == 0 {
		return m.handleSpawnSession(m.application.Session().WorkingDir)
	}

	// If the closed tab was active, switch to the next one
	if wasActive && nextActiveID != "" {
		return m.handleSwitchTab(nextActiveID)
	}

	return m, nil
}

// handleWindowResize handles window resize.
func (m *Model) handleWindowResize(width, height int) tea.Cmd {
	var cmds []tea.Cmd

	// Reserve space for tab bar and status bar
	tabBarHeight := m.tabBar.Height()

	m.statusBar.SetWidth(width)
	statusBarHeight := 1
	if view := m.statusBar.View(); view != "" {
		statusBarHeight = lipgloss.Height(view)
	}

	m.width, m.height = width, height-tabBarHeight-statusBarHeight

	if !m.ready {
		m.ready = true
	}

	// Update tab bar width
	m.tabBar.SetWidth(width)

	// Update dialog (uses full window dimensions for overlay positioning)
	u, cmd := m.dialogMgr.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.dialogMgr = u.(dialog.Manager)
	cmds = append(cmds, cmd)

	// Update chat page
	cmd = m.chatPage.SetSize(m.width, m.height)
	cmds = append(cmds, cmd)

	// Update completion manager with editor height for popup positioning
	m.completions.SetEditorBottom(m.chatPage.GetInputHeight())
	m.completions.Update(tea.WindowSizeMsg{Width: width, Height: height})

	// Update notification
	m.notification.SetSize(m.width, m.height)

	return tea.Batch(cmds...)
}

// Help returns help information for the status bar.
func (m *Model) Help() help.KeyMap {
	return core.NewSimpleHelp(m.Bindings())
}

// Bindings returns the key bindings shown in the status bar.
func (m *Model) Bindings() []key.Binding {
	quitBinding := key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("Ctrl+c", "quit"),
	)
	bindings := []key.Binding{quitBinding}
	bindings = append(bindings, m.tabBar.Bindings()...)
	bindings = append(bindings, m.chatPage.Bindings()...)
	return bindings
}

// handleKeyPress handles all keyboard input with proper priority routing.
func (m *Model) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Tab bar keys first (Ctrl+Tab, Ctrl+Shift+Tab, Ctrl+W)
	if cmd := m.tabBar.Update(msg); cmd != nil {
		return m, cmd
	}

	// Dialog gets priority when open
	if m.dialogMgr.Open() {
		u, cmd := m.dialogMgr.Update(msg)
		m.dialogMgr = u.(dialog.Manager)
		return m, cmd
	}

	// Completion popup gets priority when open
	if m.completions.Open() {
		if core.IsNavigationKey(msg) {
			u, cmd := m.completions.Update(msg)
			m.completions = u.(completion.Manager)
			return m, cmd
		}
		// For all other keys (typing), send to both completion (for filtering) and editor
		var cmds []tea.Cmd
		u, completionCmd := m.completions.Update(msg)
		m.completions = u.(completion.Manager)
		cmds = append(cmds, completionCmd)

		updated, cmd := m.chatPage.Update(msg)
		m.chatPage = updated.(chat.Page)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)
	}

	// Global keyboard shortcuts
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewExitConfirmationDialog(),
		})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+z"))):
		return m, tea.Suspend

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+k"))):
		categories := commands.BuildCommandCategories(context.Background(), m.application)
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewCommandPaletteDialog(categories),
		})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+y"))):
		return m, core.CmdHandler(messages.ToggleYoloMsg{})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+o"))):
		return m, core.CmdHandler(messages.ToggleHideToolResultsMsg{})

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return m.handleCycleAgent()

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+m"))):
		return m.handleOpenModelPicker()

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+x"))):
		return m, core.CmdHandler(messages.ClearQueueMsg{})
	}

	// Forward to chat page
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return m, cmd
}

// handleMouseClick routes mouse clicks to the tab bar or chat page.
func (m *Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	tabBarHeight := m.tabBar.Height()

	// Route clicks on the tab bar row
	if msg.Y < tabBarHeight {
		if cmd := m.tabBar.Update(msg); cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	// Dialogs use full-window coordinates (they're positioned over the entire screen)
	if m.dialogMgr.Open() {
		u, cmd := m.dialogMgr.Update(msg)
		m.dialogMgr = u.(dialog.Manager)
		return m, cmd
	}

	// Adjust Y for the chat page (which starts below the tab bar)
	msg.Y -= tabBarHeight
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return m, cmd
}

// handleMouseMotion routes mouse motion events with adjusted coordinates.
func (m *Model) handleMouseMotion(msg tea.MouseMotionMsg) (tea.Model, tea.Cmd) {
	// Ignore motion on the tab bar row.
	if msg.Y < m.tabBar.Height() {
		return m, nil
	}

	if m.dialogMgr.Open() {
		u, cmd := m.dialogMgr.Update(msg)
		m.dialogMgr = u.(dialog.Manager)
		return m, cmd
	}

	msg.Y -= m.tabBar.Height()
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return m, cmd
}

// handleMouseRelease routes mouse release events with adjusted coordinates.
func (m *Model) handleMouseRelease(msg tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	// Discard releases on the tab bar row — only clicks are meaningful there.
	if msg.Y < m.tabBar.Height() {
		return m, nil
	}

	if m.dialogMgr.Open() {
		u, cmd := m.dialogMgr.Update(msg)
		m.dialogMgr = u.(dialog.Manager)
		return m, cmd
	}

	msg.Y -= m.tabBar.Height()
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return m, cmd
}

// handleMouseWheel routes mouse wheel events with adjusted coordinates.
func (m *Model) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	if m.dialogMgr.Open() {
		u, cmd := m.dialogMgr.Update(msg)
		m.dialogMgr = u.(dialog.Manager)
		return m, cmd
	}

	msg.Y -= m.tabBar.Height()
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return m, cmd
}

// handleWheelCoalesced routes coalesced wheel events with adjusted coordinates.
func (m *Model) handleWheelCoalesced(msg messages.WheelCoalescedMsg) (tea.Model, tea.Cmd) {
	if msg.Delta == 0 {
		return m, nil
	}

	if m.dialogMgr.Open() {
		// Convert coalesced delta back to individual MouseWheelMsg events
		// so dialogs can process them (they handle tea.MouseWheelMsg, not WheelCoalescedMsg).
		steps := msg.Delta
		button := tea.MouseWheelDown
		if steps < 0 {
			steps = -steps
			button = tea.MouseWheelUp
		}
		var cmds []tea.Cmd
		for range steps {
			u, cmd := m.dialogMgr.Update(tea.MouseWheelMsg{X: msg.X, Y: msg.Y, Button: button})
			m.dialogMgr = u.(dialog.Manager)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	}

	msg.Y -= m.tabBar.Height()
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return m, cmd
}

// View renders the model.
func (m *Model) View() tea.View {
	windowTitle := m.windowTitle()

	if m.err != nil {
		return toFullscreenView(styles.ErrorStyle.Render(m.err.Error()), windowTitle)
	}

	if !m.ready {
		return toFullscreenView(
			styles.CenterStyle.
				Width(m.wWidth).
				Height(m.wHeight).
				Render(styles.MutedStyle.Render("Loading…")),
			windowTitle,
		)
	}

	// Render tab bar
	tabBarView := m.tabBar.View()

	// Render chat page
	pageView := m.chatPage.View()

	// Render status bar
	statusBarView := m.statusBar.View()

	// Combine tab bar at top, chat page in middle, status bar at bottom
	baseView := lipgloss.JoinVertical(lipgloss.Top, tabBarView, pageView, statusBarView)

	// Handle overlays
	hasOverlays := m.dialogMgr.Open() || m.notification.Open() || m.completions.Open()

	if hasOverlays {
		baseLayer := lipgloss.NewLayer(baseView)
		var allLayers []*lipgloss.Layer
		allLayers = append(allLayers, baseLayer)

		if m.dialogMgr.Open() {
			dialogLayers := m.dialogMgr.GetLayers()
			allLayers = append(allLayers, dialogLayers...)
		}

		if m.notification.Open() {
			allLayers = append(allLayers, m.notification.GetLayer())
		}

		if m.completions.Open() {
			allLayers = append(allLayers, m.completions.GetLayers()...)
		}

		canvas := lipgloss.NewCanvas(allLayers...)
		return toFullscreenView(canvas.Render(), windowTitle)
	}

	return toFullscreenView(baseView, windowTitle)
}

// windowTitle returns the terminal window title.
func (m *Model) windowTitle() string {
	if sessionTitle := m.sessionState.SessionTitle(); sessionTitle != "" {
		return sessionTitle + " - cagent"
	}
	return "cagent"
}

func toFullscreenView(content, windowTitle string) tea.View {
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.BackgroundColor = styles.Background
	view.WindowTitle = windowTitle
	return view
}
