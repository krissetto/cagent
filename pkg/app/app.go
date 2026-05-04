package app

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/export"
	"github.com/docker/docker-agent/pkg/app/transcript"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type App struct {
	runtime                runtime.Runtime
	session                *session.Session
	firstMessage           *string
	firstMessageAttach     string
	queuedMessages         []string
	events                 chan tea.Msg
	throttleDuration       time.Duration
	cancel                 context.CancelFunc
	currentAgentModel      string                  // Tracks the current agent's model ID from AgentInfoEvent
	exitAfterFirstResponse bool                    // Exit TUI after first assistant response completes
	titleGenerating        atomic.Bool             // True when title generation is in progress
	titleGen               *sessiontitle.Generator // Title generator for local runtime (nil for remote)

	// Attached-tab mode: the app renders and sends messages for an already-live
	// descendant session owned by another runtime loop. In this mode Run() does
	// not start a new RunStream; it forwards user turns via attachedSend and the
	// transcript is driven entirely by the long-lived live-event subscription.
	attached     bool
	attachedSend func(context.Context, runtime.QueuedMessage) error

	// busAvailable is true when the runtime supports [runtime.SessionObserverSubscriber].
	// When true, session-scoped events are consumed from the event bus subscription
	// (set up in New), and [App.Run] drains RunStream silently. When false (e.g.
	// remote runtime), [App.Run] falls back to forwarding from RunStream's return
	// channel.
	busAvailable bool
	// busCancelSub cancels the current per-session bus subscription goroutine;
	// replaced on each call to resubscribeBus.
	busCancelSub context.CancelFunc
}

// Opt is an option for creating a new App.
type Opt func(*App)

// WithFirstMessage sets the first message to send.
func WithFirstMessage(msg string) Opt {
	return func(a *App) {
		a.firstMessage = &msg
	}
}

// WithFirstMessageAttachment sets the attachment path for the first message.
func WithFirstMessageAttachment(path string) Opt {
	return func(a *App) {
		a.firstMessageAttach = path
	}
}

// WithExitAfterFirstResponse configures the app to exit after the first assistant response.
func WithExitAfterFirstResponse() Opt {
	return func(a *App) {
		a.exitAfterFirstResponse = true
	}
}

// WithQueuedMessages sets messages to be queued after the first message is sent.
// These messages will be delivered to the TUI as SendMsg events, which the
// chat page will queue and process sequentially after each agent response.
func WithQueuedMessages(msgs []string) Opt {
	return func(a *App) {
		a.queuedMessages = msgs
	}
}

// WithTitleGenerator sets the title generator for local title generation.
// If not set, title generation will be handled by the runtime (for remote) or skipped.
func WithTitleGenerator(gen *sessiontitle.Generator) Opt {
	return func(a *App) {
		a.titleGen = gen
	}
}

func New(ctx context.Context, rt runtime.Runtime, sess *session.Session, opts ...Opt) *App {
	app := &App{
		runtime:          rt,
		session:          sess,
		events:           make(chan tea.Msg, 128),
		throttleDuration: 50 * time.Millisecond, // Throttle rapid events
	}

	for _, opt := range opts {
		opt(app)
	}

	// Emit startup info (agent, team, tools) through the events channel.
	// This runs in the background so the TUI can start immediately while
	// slow operations (like MCP tool loading) complete asynchronously.
	go func() {
		startupEvents := make(chan runtime.Event, 10)
		go func() {
			defer close(startupEvents)
			rt.EmitStartupInfo(ctx, sess, startupEvents)
		}()
		for event := range startupEvents {
			select {
			case app.events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Subscribe to tool list changes so the sidebar updates immediately
	// when an MCP server adds or removes tools (outside of a RunStream).
	if tcs, ok := rt.(runtime.ToolsChangeSubscriber); ok {
		tcs.OnToolsChanged(func(event runtime.Event) {
			select {
			case app.events <- event:
			case <-ctx.Done():
			}
		})
	}

	// Subscribe to the session's event bus as the single source of session-
	// scoped events. This is the same path attached tabs use (via
	// LiveEventSource), so root and attached tabs now share one event
	// transport: the runtime's per-session bus is the single source of truth.
	//
	// Tracks SessionTitleEvent so background title generation can clear its
	// in-flight flag without needing a parallel watcher in [App.Run].
	//
	// When the runtime does not support [runtime.SessionObserverSubscriber]
	// (e.g. some remote configurations), no subscription is started and
	// [App.Run] falls back to forwarding events from RunStream's return
	// channel (see fallbackForwardRunStream).
	if _, ok := rt.(runtime.SessionObserverSubscriber); ok {
		app.busAvailable = true
		app.resubscribeBus(ctx, sess.ID)
	}

	return app
}

// NewAttached builds an App that renders a live descendant session using the
// normal chat UI. Unlike [New], it does not own a runtime loop itself; it is
// fed by a long-lived live-event subscription and forwards user turns back into
// the already-running descendant loop through attachedSend.
func NewAttached(ctx context.Context, rt runtime.Runtime, sess *session.Session, node runtime.LiveSessionNode, opts ...Opt) *App {
	app := &App{
		runtime:          rt,
		session:          sess,
		events:           make(chan tea.Msg, 128),
		throttleDuration: 50 * time.Millisecond,
		attached:         true,
	}
	for _, opt := range opts {
		opt(app)
	}

	if tree, ok := rt.(runtime.SessionTreeProvider); ok && liveNodeWritable(node) {
		app.attachedSend = func(ctx context.Context, msg runtime.QueuedMessage) error {
			return tree.FollowUpSessionByID(sess.ID, msg)
		}
	}

	// Seed the sidebar/chat with basic session metadata immediately; subsequent
	// live events keep everything up to date.
	go func() {
		// Attach the live event subscription BEFORE emitting startup metadata.
		//
		// The runtime's event bus does not replay past events, so any event
		// published between tab-open and subscribe is lost to this attached
		// tab. Startup emission (EmitSessionStartupInfo) can itself be slow
		// because it starts the child agent's toolsets progressively — MCP
		// servers alone can add seconds of latency before we would otherwise
		// attach. In that window a subagent's first-turn title generation
		// often completes and publishes SessionTitleEvent, so deferring the
		// attach means attached tabs get stuck on the sidebar's default
		// "New session" label. Attaching here lets the subscription buffer
		// those events so they are delivered in order once we start
		// forwarding below.
		var (
			liveStream <-chan runtime.Event
			snapshot   runtime.StreamingSnapshot
		)
		switch src := rt.(type) {
		case runtime.LiveEventSourceWithSnapshot:
			stream, snap, err := src.AttachLiveSessionWithSnapshot(ctx, sess.ID)
			if err != nil {
				select {
				case app.events <- runtime.Error(fmt.Sprintf("failed to attach live session: %v", err)):
				case <-ctx.Done():
				}
				return
			}
			liveStream = stream
			snapshot = snap
		case runtime.LiveEventSource:
			stream, err := src.AttachLiveSession(ctx, sess.ID)
			if err != nil {
				select {
				case app.events <- runtime.Error(fmt.Sprintf("failed to attach live session: %v", err)):
				case <-ctx.Done():
				}
				return
			}
			liveStream = stream
		}

		if emitter, ok := rt.(runtime.SessionStartupEmitter); ok {
			startupEvents := make(chan runtime.Event, 16)
			go func() {
				defer close(startupEvents)
				emitter.EmitSessionStartupInfo(ctx, sess, node.AgentName, startupEvents)
			}()
			for event := range startupEvents {
				select {
				case app.events <- event:
				case <-ctx.Done():
					return
				}
			}
		} else {
			// Fallback for runtimes (currently remote) that cannot yet emit a
			// session-scoped startup batch. Keep a minimal seed so the normal
			// chat UI remains usable even if the sidebar is less rich.
			select {
			case app.events <- runtime.TeamInfo([]runtime.AgentDetails{{Name: node.AgentName}}, node.AgentName):
			case <-ctx.Done():
				return
			}
			select {
			case app.events <- runtime.AgentInfo(node.AgentName, "", "", ""):
			case <-ctx.Done():
				return
			}
		}

		// Seed the session title from the freshest available snapshot. The
		// tab-open path captured `node.Title` just before spawning this
		// goroutine, but subagent title generation runs asynchronously off
		// the child loop and may have updated the handle title in the
		// meantime. Re-reading it here closes that race without relying on
		// the non-replaying event bus to re-deliver the original
		// SessionTitleEvent. When the title was already set before tab-open
		// (e.g. seeded via StartConfig.Title) this is simply idempotent.
		title := node.Title
		if tp, ok := rt.(runtime.SessionTreeProvider); ok {
			if latest, found := tp.LiveSessionNode(sess.ID); found && latest.Title != "" {
				title = latest.Title
			}
		}
		if title != "" {
			select {
			case app.events <- runtime.SessionTitle(sess.ID, title):
			case <-ctx.Done():
				return
			}
		}

		// Replay any in-progress assistant content that had already streamed
		// before this tab attached. The subscription and snapshot were
		// captured atomically by LiveEventSourceWithSnapshot, so these
		// synthetic events are the exact prefix the attached tab missed;
		// future live deltas continue seamlessly from here.
		if snapshot.HasContent() {
			if snapshot.ReasoningContent != "" {
				select {
				case app.events <- runtime.AgentChoiceReasoning(snapshot.AgentName, sess.ID, snapshot.ReasoningContent):
				case <-ctx.Done():
					return
				}
			}
			if snapshot.Content != "" {
				select {
				case app.events <- runtime.AgentChoice(snapshot.AgentName, sess.ID, snapshot.Content):
				case <-ctx.Done():
					return
				}
			}
		}

		if liveStream != nil {
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-liveStream:
					if !ok {
						return
					}
					select {
					case app.events <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return app
}

func (a *App) resubscribeBus(ctx context.Context, sessionID string) {
	if a.busCancelSub != nil {
		a.busCancelSub()
		a.busCancelSub = nil
	}

	obs, ok := a.runtime.(runtime.SessionObserverSubscriber)
	if !ok || sessionID == "" {
		return
	}

	subCtx, cancel := context.WithCancel(ctx)
	a.busCancelSub = cancel

	go func() {
		sub := obs.SubscribeSession(subCtx, sessionID, 128)
		defer sub.Cancel()
		for ev := range sub.Events {
			if _, ok := ev.(*runtime.SessionTitleEvent); ok {
				a.titleGenerating.Store(false)
			}
			select {
			case a.events <- ev:
			case <-subCtx.Done():
				return
			}
		}
	}()
}

func (a *App) SendFirstMessage() tea.Cmd {
	if a.firstMessage == nil {
		return nil
	}

	cmds := []tea.Cmd{
		func() tea.Msg {
			// Use the shared PrepareUserMessage function for consistent attachment handling
			userMsg := cli.PrepareUserMessage(context.Background(), a.runtime, *a.firstMessage, a.firstMessageAttach)

			// If the message has multi-content (attachments), we need to handle it specially
			if len(userMsg.Message.MultiContent) > 0 {
				return messages.SendAttachmentMsg{
					Content: userMsg,
				}
			}

			return messages.SendMsg{
				Content: userMsg.Message.Content,
			}
		},
	}

	// Queue additional messages to be sent after the first one.
	// The TUI's message queue will hold them until the agent finishes
	// processing the previous message.
	for _, msg := range a.queuedMessages {
		cmds = append(cmds, func() tea.Msg {
			return messages.SendMsg{
				Content: msg,
			}
		})
	}

	return tea.Sequence(cmds...)
}

// CurrentAgentTools returns the tools available to the current agent.
func (a *App) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	if a.attached {
		return nil, nil
	}
	return a.runtime.CurrentAgentTools(ctx)
}

// CurrentAgentCommands returns the commands for the active agent
func (a *App) CurrentAgentCommands(ctx context.Context) types.Commands {
	if a.attached {
		return nil
	}
	return a.runtime.CurrentAgentInfo(ctx).Commands
}

// CurrentAgentSkills returns the available skills if skills are enabled for the current agent.
func (a *App) CurrentAgentSkills() []skills.Skill {
	if a.attached {
		return nil
	}
	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return nil
	}
	return st.Skills()
}

// ResolveSkillCommand checks if the input matches a skill slash command (e.g. /skill-name args).
// If matched, it reads the skill content and returns the resolved prompt. Otherwise returns "".
func (a *App) ResolveSkillCommand(ctx context.Context, input string) (string, error) {
	if a.attached {
		return "", nil
	}
	if !strings.HasPrefix(input, "/") {
		return "", nil
	}

	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return "", nil
	}

	cmd, arg, _ := strings.Cut(input[1:], " ")
	arg = strings.TrimSpace(arg)

	for _, skill := range st.Skills() {
		if skill.Name != cmd {
			continue
		}

		content, err := st.ReadSkillContent(ctx, skill.Name)
		if err != nil {
			return "", fmt.Errorf("reading skill %q: %w", skill.Name, err)
		}

		if arg != "" {
			return fmt.Sprintf("Use the following skill.\n\nUser's request: %s\n\n<skill name=%q>\n%s\n</skill>", arg, skill.Name, content), nil
		}
		return fmt.Sprintf("Use the following skill.\n\n<skill name=%q>\n%s\n</skill>", skill.Name, content), nil
	}

	return "", nil
}

// ResolveInput resolves the user input by trying skill commands first,
// then agent commands. Returns the resolved content ready to send to the agent.
func (a *App) ResolveInput(ctx context.Context, input string) string {
	if a.attached {
		return input
	}
	if resolved, err := a.ResolveSkillCommand(ctx, input); err != nil {
		return fmt.Sprintf("Error loading skill: %v", err)
	} else if resolved != "" {
		return resolved
	}

	return a.ResolveCommand(ctx, input)
}

// CurrentAgentModel returns the model ID for the current agent.
// Returns the tracked model from AgentInfoEvent, or falls back to session overrides.
// Returns empty string if no model information is available (fail-open scenario).
func (a *App) CurrentAgentModel() string {
	if a.currentAgentModel != "" {
		return a.currentAgentModel
	}
	// Fallback to session overrides
	if a.session != nil && a.session.AgentModelOverrides != nil {
		agentName := a.runtime.CurrentAgentName()
		if modelRef, ok := a.session.AgentModelOverrides[agentName]; ok {
			return modelRef
		}
	}
	return ""
}

// TrackCurrentAgentModel updates the tracked model ID for the current agent.
// This is called when AgentInfoEvent is received from the runtime.
func (a *App) TrackCurrentAgentModel(model string) {
	a.currentAgentModel = model
}

// CurrentMCPPrompts returns the available MCP prompts for the active agent
func (a *App) CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo {
	if a.attached {
		return map[string]mcptools.PromptInfo{}
	}
	return a.runtime.CurrentMCPPrompts(ctx)
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content
func (a *App) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	if a.attached {
		return "", errors.New("MCP prompts are unavailable in attached child-session tabs")
	}
	return a.runtime.ExecuteMCPPrompt(ctx, promptName, arguments)
}

// ResolveCommand converts /command to its prompt text
func (a *App) ResolveCommand(ctx context.Context, userInput string) string {
	return runtime.ResolveCommand(ctx, a.runtime, userInput)
}

// EmitStartupInfo emits initial agent, team, and toolset information to the provided channel
func (a *App) EmitStartupInfo(ctx context.Context, events chan runtime.Event) {
	a.runtime.EmitStartupInfo(ctx, a.session, events)
}

// Run one agent loop
func (a *App) Run(ctx context.Context, cancel context.CancelFunc, message string, attachments []messages.Attachment) {
	a.cancel = cancel

	if a.attached {
		go func() {
			qm := a.buildQueuedMessage(ctx, message, attachments)
			if a.attachedSend == nil {
				a.sendEvent(ctx, runtime.Error("attached session is not writable"))
				return
			}
			if err := a.attachedSend(ctx, qm); err != nil {
				a.sendEvent(ctx, runtime.Error(fmt.Sprintf("failed to send message to attached session: %v", err)))
			}
		}()
		return
	}

	// If this is the first message and no title exists, start local title generation
	if a.session.Title == "" && a.titleGen != nil {
		a.titleGenerating.Store(true)
		go a.generateTitle(ctx, []string{message})
	}

	go func() {
		a.session.AddMessage(session.UserMessage(message, a.buildQueuedMessage(ctx, message, attachments).MultiContent...))
		stream := a.runtime.RunStream(ctx, a.session)
		if a.busAvailable {
			// Events are delivered to app.events through the session-bus
			// subscription started in New(). We only need to drain the
			// RunStream channel so the goroutine inside wrapEventsForObservers
			// can exit cleanly when the stream ends.
			for range stream {
			}
		} else {
			// Fallback for runtimes without a session bus (remote): forward
			// events from the RunStream return channel the old-fashioned way.
			a.fallbackForwardRunStream(ctx, stream)
		}
	}()
}

// buildQueuedMessage prepares a runtime.QueuedMessage from user text plus any
// attachments. It mirrors the normal Run path's attachment expansion so the
// same payload shape can be used for normal sessions, parent follow-ups, and
// attached child-session tabs.
func (a *App) buildQueuedMessage(ctx context.Context, message string, attachments []messages.Attachment) runtime.QueuedMessage {
	if len(attachments) == 0 {
		return runtime.QueuedMessage{Content: message}
	}

	// Build a single text block that includes the user's message and any
	// inlined text attachments, while binary attachments are emitted as
	// structured MessageParts.
	var textBuilder strings.Builder
	textBuilder.WriteString(message)
	var binaryParts []chat.MessagePart

	for _, att := range attachments {
		switch {
		case att.FilePath != "":
			a.processFileAttachment(ctx, att, &textBuilder, &binaryParts)
		case att.Content != "":
			a.processInlineAttachment(att, &textBuilder)
		default:
			slog.Debug("skipping attachment with no file path or content", "name", att.Name)
		}
	}

	multiContent := []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: textBuilder.String()}}
	multiContent = append(multiContent, binaryParts...)
	return runtime.QueuedMessage{Content: message, MultiContent: multiContent}
}

// processFileAttachment reads a file from disk, classifies it, and either
// appends its text content to textBuilder or adds a binary part to binaryParts.
func (a *App) processFileAttachment(ctx context.Context, att messages.Attachment, textBuilder *strings.Builder, binaryParts *[]chat.MessagePart) {
	absPath := att.FilePath

	fi, err := os.Stat(absPath)
	if err != nil {
		var reason string
		switch {
		case os.IsNotExist(err):
			reason = "file does not exist"
		case os.IsPermission(err):
			reason = "permission denied"
		default:
			reason = fmt.Sprintf("cannot access file: %v", err)
		}
		slog.Warn("skipping attachment", "path", absPath, "reason", reason)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, reason), ""))
		return
	}

	if !fi.Mode().IsRegular() {
		slog.Warn("skipping attachment: not a regular file", "path", absPath, "mode", fi.Mode().String())
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: not a regular file", att.Name), ""))
		return
	}

	const maxAttachmentSize = 100 * 1024 * 1024 // 100MB
	if fi.Size() > maxAttachmentSize {
		slog.Warn("skipping attachment: file too large", "path", absPath, "size", fi.Size(), "max", maxAttachmentSize)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: file too large (max 100MB)", att.Name), ""))
		return
	}

	mimeType := chat.DetectMimeType(absPath)

	switch {
	case chat.IsTextFile(absPath):
		if fi.Size() > chat.MaxInlineFileSize {
			slog.Warn("skipping attachment: text file too large to inline", "path", absPath, "size", fi.Size(), "max", chat.MaxInlineFileSize)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: text file too large to inline (max 5MB)", att.Name), ""))
			return
		}
		content, err := chat.ReadFileForInline(absPath)
		if err != nil {
			slog.Warn("skipping attachment: failed to read file", "path", absPath, "error", err)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: failed to read file", att.Name), ""))
			return
		}
		textBuilder.WriteString("\n\n")
		textBuilder.WriteString(content)

	case chat.IsSupportedMimeType(mimeType):
		if chat.IsImageMimeType(mimeType) {
			// Read, resize if needed, and inline as base64 data URL.
			// This works across all providers (not just Anthropic's File API).
			imgData, readErr := os.ReadFile(absPath)
			if readErr != nil {
				slog.Warn("skipping attachment: failed to read image", "path", absPath, "error", readErr)
				a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: failed to read image", att.Name), ""))
				return
			}
			resized, resizeErr := chat.ResizeImage(imgData, mimeType)
			if resizeErr != nil {
				// Don't bypass security checks - reject the file if resize failed
				slog.Warn("skipping attachment: image resize failed", "path", absPath, "error", resizeErr)
				a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, resizeErr), ""))
				return
			}
			dataURL := fmt.Sprintf("data:%s;base64,%s", resized.MimeType, base64.StdEncoding.EncodeToString(resized.Data))
			*binaryParts = append(*binaryParts, chat.MessagePart{
				Type: chat.MessagePartTypeImageURL,
				ImageURL: &chat.MessageImageURL{
					URL:    dataURL,
					Detail: chat.ImageURLDetailAuto,
				},
			})
			if note := chat.FormatDimensionNote(resized); note != "" {
				textBuilder.WriteString("\n" + note)
			}
		} else {
			// Non-image supported types (e.g. PDF) use the file upload path.
			*binaryParts = append(*binaryParts, chat.MessagePart{
				Type: chat.MessagePartTypeFile,
				File: &chat.MessageFile{
					Path:     absPath,
					MimeType: mimeType,
				},
			})
		}

	default:
		slog.Warn("skipping attachment: unsupported file type", "path", absPath, "mime_type", mimeType)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: unsupported file type", att.Name), ""))
	}
}

// sendEvent sends an event to the TUI, respecting context cancellation to
// avoid blocking on the channel when the consumer has stopped reading.
func (a *App) sendEvent(ctx context.Context, event tea.Msg) {
	select {
	case a.events <- event:
	case <-ctx.Done():
	}
}

// fallbackForwardRunStream drains a RunStream return channel and forwards
// events to app.events. This is the pre-bus-subscription path used only when
// the runtime does not support [runtime.SessionObserverSubscriber] (e.g.
// remote/HTTP runtimes). Local runtimes with a session bus never call this;
// see App.Run for the bus-available path.
func (a *App) fallbackForwardRunStream(ctx context.Context, stream <-chan runtime.Event) {
	for event := range stream {
		if ctx.Err() != nil {
			// Context cancelled: suppress most events but always forward
			// StreamStoppedEvent so the supervisor can mark the session as
			// no longer running.
			if _, ok := event.(*runtime.StreamStoppedEvent); ok {
				a.sendEvent(context.Background(), event)
			}
			continue
		}
		if _, ok := event.(*runtime.SessionTitleEvent); ok {
			a.titleGenerating.Store(false)
		}
		a.sendEvent(ctx, event)
	}
}

// processInlineAttachment handles content that is already in memory (e.g. pasted
// text). The content is appended to textBuilder wrapped in an XML tag for context.
func (a *App) processInlineAttachment(att messages.Attachment, textBuilder *strings.Builder) {
	textBuilder.WriteString("\n\n")
	fmt.Fprintf(textBuilder, "<attached_file path=%q>\n%s\n</attached_file>", att.Name, att.Content)
}

// RunWithMessage runs the agent loop with a pre-constructed message.
// This is used for special cases like image attachments.
func (a *App) RunWithMessage(ctx context.Context, cancel context.CancelFunc, msg *session.Message) {
	a.cancel = cancel

	// If this is the first message and no title exists, start local title generation
	if a.session.Title == "" && a.titleGen != nil {
		a.titleGenerating.Store(true)
		// Extract text content from the message for title generation
		userMessage := msg.Message.Content
		if userMessage == "" && len(msg.Message.MultiContent) > 0 {
			for _, part := range msg.Message.MultiContent {
				if part.Type == chat.MessagePartTypeText {
					userMessage = part.Text
					break
				}
			}
		}
		go a.generateTitle(ctx, []string{userMessage})
	}

	go func() {
		a.session.AddMessage(msg)
		stream := a.runtime.RunStream(ctx, a.session)
		if a.busAvailable {
			for range stream {
			}
		} else {
			a.fallbackForwardRunStream(ctx, stream)
		}
	}()
}

func (a *App) RunBangCommand(ctx context.Context, command string) {
	if a.attached {
		a.events <- runtime.Error("shell bang commands are unavailable in attached child-session tabs")
		return
	}
	command = strings.TrimSpace(command)
	if command == "" {
		a.events <- runtime.ShellOutput("Error: empty command")
		return
	}

	shell, argsPrefix := shellpath.DetectShell()
	out, err := exec.CommandContext(ctx, shell, append(argsPrefix, command)...).CombinedOutput()
	output := "$ " + command + "\n" + string(out)
	if err != nil && len(out) == 0 {
		output = "$ " + command + "\nError: " + err.Error()
	}
	a.events <- runtime.ShellOutput(output)
}

// SubscribeWith subscribes to app events using a custom send function.
// This allows callers to wrap or transform messages before sending them
// to the Bubble Tea program (e.g. to tag events with a session ID for routing).
func (a *App) SubscribeWith(ctx context.Context, send func(tea.Msg)) {
	throttledChan := a.throttleEvents(ctx, a.events)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-throttledChan:
			if !ok {
				return
			}

			send(msg)
		}
	}
}

// Resume resumes the runtime with the given confirmation request
func (a *App) Resume(req runtime.ResumeRequest) {
	a.runtime.Resume(context.Background(), req)
}

// InterruptAttachedSession cancels the currently-running turn of the attached
// live session. It is only meaningful in attached mode: the attached App
// doesn't own a runtime loop of its own, so the normal per-stream cancel used
// by owned sessions has nothing local to cancel. Instead, we reach into the
// runtime's live-tree surface and ask it to interrupt the descendant that
// this App is attached to.
//
// Returns an error when the runtime doesn't support live-session control (e.g.
// certain remote-runtime configurations), when the session is no longer live,
// or when the underlying runtime refuses the interrupt.
func (a *App) InterruptAttachedSession() error {
	if !a.attached {
		return errors.New("interrupt is only meaningful for attached live sessions")
	}
	if a.session == nil {
		return errors.New("no active session")
	}
	tree, ok := a.runtime.(runtime.SessionTreeProvider)
	if !ok {
		return errors.New("runtime does not support live-session control")
	}
	return tree.InterruptSessionByID(a.session.ID)
}

// FollowUp routes a plain-text user message into the runtime's follow-up
// queue so it is picked up as a fresh turn after the current one finishes
// (or while the parent loop is idle-waiting on live subagents). Unlike
// [Run], this never cancels or restarts the in-flight stream, which is
// exactly what we want when the user keeps typing while subagents are
// still working in the background.
//
// Returns an error when the runtime is not running or its queue is full.
func (a *App) FollowUp(content string) error {
	return a.FollowUpWithAttachments(content, nil)
}

// FollowUpWithAttachments is the multi-modal variant of [FollowUp]. It mirrors
// the normal send path's attachment expansion so file/image attachments flow
// through to the runtime queue exactly like they do on a fresh Run.
func (a *App) FollowUpWithAttachments(content string, attachments []messages.Attachment) error {
	content = strings.TrimSpace(content)
	if content == "" && len(attachments) == 0 {
		return nil
	}
	qm := a.buildQueuedMessage(context.Background(), content, attachments)
	return a.runtime.FollowUp(qm)
}

// ResumeElicitation resumes an elicitation request with the given action and content
func (a *App) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	return a.runtime.ResumeElicitation(ctx, action, content)
}

func (a *App) NewSession() {
	if a.attached {
		return
	}
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	// Preserve user-controlled session flags
	// so they don't reset to default on /new
	var opts []session.Opt
	if a.session != nil {
		opts = append(opts,
			session.WithToolsApproved(a.session.ToolsApproved),
			session.WithHideToolResults(a.session.HideToolResults),
			session.WithWorkingDir(a.session.WorkingDir),
		)
	}
	a.session = session.New(opts...)
	if a.busAvailable {
		a.resubscribeBus(context.Background(), a.session.ID)
	}
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""

	// Re-emit startup info so the sidebar shows agent/tools info in the new session
	a.reEmitStartupInfo(context.Background())
}

// reEmitStartupInfo resets and re-emits startup info (agent, team, tools)
// through the events channel so the sidebar updates.
func (a *App) reEmitStartupInfo(ctx context.Context) {
	a.runtime.ResetStartupInfo()
	go func() {
		startupEvents := make(chan runtime.Event, 10)
		go func() {
			defer close(startupEvents)
			a.runtime.EmitStartupInfo(ctx, a.session, startupEvents)
		}()
		for event := range startupEvents {
			select {
			case a.events <- event:
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}

func (a *App) Session() *session.Session {
	return a.session
}

// PermissionsInfo returns combined permissions info from team and session.
// Returns nil if no permissions are configured at either level.
func (a *App) PermissionsInfo() *runtime.PermissionsInfo {
	if a.attached {
		return nil
	}
	// Get team-level permissions from runtime
	teamPerms := a.runtime.PermissionsInfo()

	// Get session-level permissions
	var sessionPerms *runtime.PermissionsInfo
	if a.session != nil && a.session.Permissions != nil {
		if len(a.session.Permissions.Allow) > 0 || len(a.session.Permissions.Ask) > 0 || len(a.session.Permissions.Deny) > 0 {
			sessionPerms = &runtime.PermissionsInfo{
				Allow: a.session.Permissions.Allow,
				Ask:   a.session.Permissions.Ask,
				Deny:  a.session.Permissions.Deny,
			}
		}
	}

	// Return nil if no permissions configured at any level
	if teamPerms == nil && sessionPerms == nil {
		return nil
	}

	// Merge permissions, with session taking priority (listed first)
	result := &runtime.PermissionsInfo{}
	if sessionPerms != nil {
		result.Allow = append(result.Allow, sessionPerms.Allow...)
		result.Ask = append(result.Ask, sessionPerms.Ask...)
		result.Deny = append(result.Deny, sessionPerms.Deny...)
	}
	if teamPerms != nil {
		result.Allow = append(result.Allow, teamPerms.Allow...)
		result.Ask = append(result.Ask, teamPerms.Ask...)
		result.Deny = append(result.Deny, teamPerms.Deny...)
	}

	return result
}

// HasPermissions returns true if any permissions are configured (team or session level).
func (a *App) HasPermissions() bool {
	return a.PermissionsInfo() != nil
}

// SwitchAgent switches the currently active agent for subsequent user messages
func (a *App) SwitchAgent(agentName string) error {
	if a.attached {
		return errors.New("attached child-session tabs cannot switch agents")
	}
	return a.runtime.SetCurrentAgent(agentName)
}

// SetCurrentAgentModel sets the model for the current agent and persists
// the override in the session. Returns an error if model switching is not
// supported by the runtime (e.g., remote runtimes).
// Pass an empty modelRef to clear the override and use the agent's default model.
func (a *App) SetCurrentAgentModel(ctx context.Context, modelRef string) error {
	if a.attached {
		return errors.New("model switching is unavailable in attached child-session tabs")
	}
	modelSwitcher, ok := a.runtime.(runtime.ModelSwitcher)
	if !ok {
		return errors.New("model switching not supported by this runtime")
	}

	agentName := a.runtime.CurrentAgentName()

	// Set the model override on the runtime (empty modelRef clears the override)
	if err := modelSwitcher.SetAgentModel(ctx, agentName, modelRef); err != nil {
		return err
	}

	// Update the session's model overrides
	if modelRef == "" {
		// Clear the override - remove from map
		delete(a.session.AgentModelOverrides, agentName)
		slog.Debug("Cleared model override from session", "session_id", a.session.ID, "agent", agentName)
	} else {
		// Set the override
		if a.session.AgentModelOverrides == nil {
			a.session.AgentModelOverrides = make(map[string]string)
		}
		a.session.AgentModelOverrides[agentName] = modelRef
		slog.Debug("Set model override in session", "session_id", a.session.ID, "agent", agentName, "model", modelRef)

		// Track custom models (inline provider/model format) in the session
		if strings.Contains(modelRef, "/") {
			a.trackCustomModel(modelRef)
		}
	}

	// Persist the session
	if store := a.runtime.SessionStore(); store != nil {
		if err := store.UpdateSession(ctx, a.session); err != nil {
			return fmt.Errorf("failed to persist model override: %w", err)
		}
		slog.Debug("Persisted session with model override", "session_id", a.session.ID, "overrides", a.session.AgentModelOverrides)
	}

	// Re-emit startup info so the sidebar updates with the new model
	a.reEmitStartupInfo(ctx)

	return nil
}

// AvailableModels returns the list of models available for selection.
// Returns nil if model switching is not supported.
func (a *App) AvailableModels(ctx context.Context) []runtime.ModelChoice {
	modelSwitcher, ok := a.runtime.(runtime.ModelSwitcher)
	if !ok {
		return nil
	}
	models := modelSwitcher.AvailableModels(ctx)

	// Determine the currently active model for this agent
	agentName := a.runtime.CurrentAgentName()
	currentModelRef := ""
	if a.session != nil && a.session.AgentModelOverrides != nil {
		currentModelRef = a.session.AgentModelOverrides[agentName]
	}

	// Build a set of model refs already in the list
	existingRefs := make(map[string]bool)
	for _, m := range models {
		existingRefs[m.Ref] = true
	}

	// Check if current model is in the list and mark it
	currentFound := currentModelRef == ""
	for i := range models {
		if currentModelRef != "" {
			// An override is set - mark the override as current
			if models[i].Ref == currentModelRef {
				models[i].IsCurrent = true
				currentFound = true
			}
		} else {
			// No override - the default model is current
			models[i].IsCurrent = models[i].IsDefault
		}
	}

	// Add custom models from the session that aren't already in the list
	if a.session != nil {
		for _, customRef := range a.session.CustomModelsUsed {
			if existingRefs[customRef] {
				continue // Already in the list
			}
			existingRefs[customRef] = true

			providerName, modelName, _ := strings.Cut(customRef, "/")
			isCurrent := customRef == currentModelRef
			if isCurrent {
				currentFound = true
			}
			models = append(models, runtime.ModelChoice{
				Name:      customRef,
				Ref:       customRef,
				Provider:  providerName,
				Model:     modelName,
				IsDefault: false,
				IsCurrent: isCurrent,
				IsCustom:  true,
			})
		}
	}

	// If current model is a custom model not in the list, add it
	if !currentFound && strings.Contains(currentModelRef, "/") {
		providerName, modelName, _ := strings.Cut(currentModelRef, "/")
		models = append(models, runtime.ModelChoice{
			Name:      currentModelRef,
			Ref:       currentModelRef,
			Provider:  providerName,
			Model:     modelName,
			IsDefault: false,
			IsCurrent: true,
			IsCustom:  true,
		})
	}

	return models
}

// trackCustomModel adds a custom model to the session's history if not already present.
func (a *App) trackCustomModel(modelRef string) {
	if a.session == nil {
		return
	}

	// Check if already tracked
	if slices.Contains(a.session.CustomModelsUsed, modelRef) {
		return
	}

	a.session.CustomModelsUsed = append(a.session.CustomModelsUsed, modelRef)
	slog.Debug("Tracked custom model in session", "session_id", a.session.ID, "model", modelRef)
}

// SupportsModelSwitching returns true if the runtime supports model switching.
func (a *App) SupportsModelSwitching() bool {
	if a.attached {
		return false
	}
	_, ok := a.runtime.(runtime.ModelSwitcher)
	return ok
}

// ShouldExitAfterFirstResponse returns true if the app is configured to exit
// after the first assistant response completes.
func (a *App) ShouldExitAfterFirstResponse() bool {
	return a.exitAfterFirstResponse
}

func (a *App) CompactSession(ctx context.Context, additionalPrompt string) {
	if a.attached {
		a.sendEvent(ctx, runtime.Warning("Session compaction is unavailable in attached child-session tabs.", ""))
		return
	}
	sess := a.session
	if sess == nil {
		return
	}

	go func() {
		events := make(chan runtime.Event, 100)
		go func() {
			defer close(events)
			a.runtime.Summarize(ctx, sess, additionalPrompt, events)
		}()
		for event := range events {
			if ctx.Err() != nil {
				return
			}
			a.sendEvent(ctx, event)
		}
	}()
}

func (a *App) PlainTextTranscript() string {
	return transcript.PlainText(a.session)
}

// SessionStore returns the session store for browsing/loading sessions.
// Returns nil if no session store is configured.
func (a *App) SessionStore() session.Store {
	if a.attached {
		return nil
	}
	return a.runtime.SessionStore()
}

// Runtime exposes the underlying runtime implementation. This is used by the
// TUI when it needs to construct a second App view over the same live runtime
// tree (for attached child-session tabs).
func (a *App) Runtime() runtime.Runtime {
	return a.runtime
}

// LiveSessionTree returns the live session tree rooted at rootID, or nil when
// the underlying runtime does not expose session-tree observability.
func (a *App) LiveSessionTree(rootID string) []runtime.LiveSessionNode {
	provider, ok := a.runtime.(runtime.SessionTreeProvider)
	if !ok {
		return nil
	}
	return provider.LiveSessionTree(rootID)
}

// LiveEventSource returns a LiveEventSource view of the underlying runtime,
// or nil when the runtime does not support live observability (e.g. certain
// remote runtime configurations).
//
// The supervisor uses this to attach a secondary event subscription when the
// user opens a live subagent tab from the sidebar.
func (a *App) LiveEventSource() runtime.LiveEventSource {
	if src, ok := a.runtime.(runtime.LiveEventSource); ok {
		return src
	}
	return nil
}

// LiveSessionNode returns a snapshot of the live-session node with the given
// id, or ok=false when the runtime cannot resolve it. The call is a thin
// pass-through to runtime.SessionTreeProvider so callers outside pkg/runtime
// do not need to type-assert themselves.
func (a *App) LiveSessionNode(sessionID string) (runtime.LiveSessionNode, bool) {
	provider, ok := a.runtime.(runtime.SessionTreeProvider)
	if !ok {
		return runtime.LiveSessionNode{}, false
	}
	return provider.LiveSessionNode(sessionID)
}

// LiveSession resolves the in-memory session pointer for a live descendant.
// Only local runtimes can satisfy this because it exposes a direct pointer to
// mutable session state.
func (a *App) LiveSession(sessionID string) (*session.Session, bool) {
	provider, ok := a.runtime.(runtime.LiveSessionProvider)
	if !ok {
		return nil, false
	}
	return provider.LiveSession(sessionID)
}

// ReplaceSession replaces the current session with the given session.
// This is used when loading a past session. It also re-emits startup info
// so the sidebar displays the agent and tool information.
// If the session has stored model overrides, they are applied to the runtime.
func (a *App) ReplaceSession(ctx context.Context, sess *session.Session) {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.session = sess
	if a.busAvailable {
		a.resubscribeBus(ctx, sess.ID)
	}
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""

	// Apply any stored model overrides from the session
	a.applySessionModelOverrides(ctx, sess)

	// Reset and re-emit startup info so the sidebar shows agent/tools info
	a.reEmitStartupInfo(ctx)
}

// applySessionModelOverrides applies any stored model overrides from a loaded session.
func (a *App) applySessionModelOverrides(ctx context.Context, sess *session.Session) {
	if len(sess.AgentModelOverrides) == 0 {
		slog.Debug("No model overrides to apply from session", "session_id", sess.ID)
		return
	}

	// Check if runtime supports model switching
	modelSwitcher, ok := a.runtime.(runtime.ModelSwitcher)
	if !ok {
		slog.Debug("Runtime does not support model switching, skipping overrides")
		return
	}

	slog.Debug("Applying model overrides from session", "session_id", sess.ID, "overrides", sess.AgentModelOverrides)
	for agentName, modelRef := range sess.AgentModelOverrides {
		if err := modelSwitcher.SetAgentModel(ctx, agentName, modelRef); err != nil {
			// Log but don't fail - the session can still be used with default models
			slog.Warn("Failed to apply model override from session", "agent", agentName, "model", modelRef, "error", err)
			a.events <- runtime.Warning(fmt.Sprintf("Failed to apply model override for agent %q: %v", agentName, err), agentName)
		} else {
			slog.Info("Applied model override from session", "agent", agentName, "model", modelRef)
		}
	}
}

// throttleEvents buffers and merges rapid events to prevent UI flooding
func (a *App) throttleEvents(ctx context.Context, in <-chan tea.Msg) <-chan tea.Msg {
	out := make(chan tea.Msg, 128)

	go func() {
		defer close(out)

		var buffer []tea.Msg
		var timerCh <-chan time.Time

		flush := func() {
			for _, msg := range a.mergeEvents(buffer) {
				select {
				case out <- msg:
				case <-ctx.Done():
					return
				}
			}
			buffer = buffer[:0]
			timerCh = nil
		}

		for {
			select {
			case <-ctx.Done():
				return

			case msg, ok := <-in:
				if !ok {
					return
				}

				buffer = append(buffer, msg)
				if a.shouldThrottle(msg) {
					if timerCh == nil {
						timerCh = time.After(a.throttleDuration)
					}
				} else {
					flush()
				}

			case <-timerCh:
				flush()
			}
		}
	}()

	return out
}

// shouldThrottle determines if an event should be buffered/throttled
func (a *App) shouldThrottle(msg tea.Msg) bool {
	switch msg.(type) {
	case *runtime.AgentChoiceEvent:
		return true
	case *runtime.AgentChoiceReasoningEvent:
		return true
	case *runtime.PartialToolCallEvent:
		return true
	default:
		return false
	}
}

// mergeEvents merges consecutive similar events to reduce UI updates
func (a *App) mergeEvents(events []tea.Msg) []tea.Msg {
	if len(events) == 0 {
		return events
	}

	var result []tea.Msg

	// Group events by type and merge
	for i := 0; i < len(events); i++ {
		current := events[i]

		switch ev := current.(type) {
		case *runtime.AgentChoiceEvent:
			// Merge consecutive AgentChoiceEvents with same agent
			merged := ev
			for i+1 < len(events) {
				if next, ok := events[i+1].(*runtime.AgentChoiceEvent); ok && next.AgentName == ev.AgentName {
					// Concatenate content
					merged = &runtime.AgentChoiceEvent{
						Type:         ev.Type,
						Content:      merged.Content + next.Content,
						AgentContext: ev.AgentContext,
					}
					i++
				} else {
					break
				}
			}
			result = append(result, merged)

		case *runtime.AgentChoiceReasoningEvent:
			// Merge consecutive AgentChoiceReasoningEvents with same agent
			merged := ev
			for i+1 < len(events) {
				if next, ok := events[i+1].(*runtime.AgentChoiceReasoningEvent); ok && next.AgentName == ev.AgentName {
					// Concatenate content
					merged = &runtime.AgentChoiceReasoningEvent{
						Type:         ev.Type,
						Content:      merged.Content + next.Content,
						AgentContext: ev.AgentContext,
					}
					i++
				} else {
					break
				}
			}
			result = append(result, merged)

		case *runtime.PartialToolCallEvent:
			// For PartialToolCallEvent, merge consecutive events with the same tool call ID
			// by concatenating argument deltas
			latest := ev
			for i+1 < len(events) {
				if next, ok := events[i+1].(*runtime.PartialToolCallEvent); ok && next.ToolCall.ID == ev.ToolCall.ID {
					latest = &runtime.PartialToolCallEvent{
						Type: ev.Type,
						ToolCall: tools.ToolCall{
							ID:   ev.ToolCall.ID,
							Type: ev.ToolCall.Type,
							Function: tools.FunctionCall{
								Name:      cmp.Or(next.ToolCall.Function.Name, latest.ToolCall.Function.Name),
								Arguments: latest.ToolCall.Function.Arguments + next.ToolCall.Function.Arguments,
							},
						},
						ToolDefinition: cmp.Or(latest.ToolDefinition, next.ToolDefinition),
						AgentContext:   ev.AgentContext,
					}
					i++
				} else {
					break
				}
			}
			result = append(result, latest)

		default:
			// Pass through other events as-is
			result = append(result, current)
		}
	}

	return result
}

// ExportHTML exports the current session as a standalone HTML file.
// If filename is empty, a default name based on the session title and timestamp is used.
func (a *App) ExportHTML(ctx context.Context, filename string) (string, error) {
	agentInfo := a.runtime.CurrentAgentInfo(ctx)
	return export.SessionToFile(a.session, agentInfo.Description, filename)
}

// ErrTitleGenerating is returned when attempting to set a title while generation is in progress.
var ErrTitleGenerating = errors.New("title generation in progress, please wait")

// UpdateSessionTitle updates the current session's title and persists it.
// It works with both local and remote runtimes.
func (a *App) UpdateSessionTitle(ctx context.Context, title string) error {
	if a.attached {
		return errors.New("attached child-session tabs cannot edit titles")
	}
	if a.session == nil {
		return errors.New("no active session")
	}

	// Prevent manual title edits while generation is in progress
	if a.titleGenerating.Load() {
		return ErrTitleGenerating
	}

	// Persist the title through the runtime
	if err := a.runtime.UpdateSessionTitle(ctx, a.session, title); err != nil {
		return fmt.Errorf("failed to update session title: %w", err)
	}

	// Emit a SessionTitleEvent to update the UI consistently
	a.events <- runtime.SessionTitle(a.session.ID, title)
	return nil
}

// IsTitleGenerating returns true if title generation is currently in progress.
func (a *App) IsTitleGenerating() bool {
	return a.titleGenerating.Load()
}

// generateTitle generates a title using the local title generator.
// This method always clears the titleGenerating flag when done (success or failure).
// It should be called in a goroutine.
func (a *App) generateTitle(ctx context.Context, userMessages []string) {
	// Always clear the flag when done, whether success or failure
	defer a.titleGenerating.Store(false)

	if a.titleGen == nil {
		slog.Debug("No title generator available, skipping title generation")
		// Emit empty title event so the UI clears any title-generation spinner
		select {
		case a.events <- runtime.SessionTitle(a.session.ID, ""):
		case <-ctx.Done():
		}
		return
	}

	title, err := a.titleGen.Generate(ctx, a.session.ID, userMessages)
	if err != nil {
		slog.Error("Failed to generate session title", "session_id", a.session.ID, "error", err)
		// Emit empty title event so the UI clears any title-generation spinner
		select {
		case a.events <- runtime.SessionTitle(a.session.ID, ""):
		case <-ctx.Done():
		}
		return
	}

	if title == "" {
		// Emit empty title event so the UI clears any title-generation spinner
		select {
		case a.events <- runtime.SessionTitle(a.session.ID, ""):
		case <-ctx.Done():
		}
		return
	}

	// Persist the title
	if err := a.runtime.UpdateSessionTitle(ctx, a.session, title); err != nil {
		slog.Error("Failed to persist title", "session_id", a.session.ID, "error", err)
	}

	// Emit the title event to update the UI
	select {
	case a.events <- runtime.SessionTitle(a.session.ID, title):
	case <-ctx.Done():
	}
}

// RegenerateSessionTitle triggers AI-based title regeneration for the current session.
// Returns ErrTitleGenerating if a title generation is already in progress.
func (a *App) RegenerateSessionTitle(ctx context.Context) error {
	if a.attached {
		return errors.New("attached child-session tabs cannot regenerate titles")
	}
	if a.session == nil {
		return errors.New("no active session")
	}

	// Check if title generation is already in progress
	if a.titleGenerating.Load() {
		return ErrTitleGenerating
	}

	// For local runtime with title generator, use it directly
	if a.titleGen != nil {
		a.titleGenerating.Store(true)

		// Collect user messages for title generation
		var userMessages []string
		for _, msg := range a.session.GetAllMessages() {
			if msg.Message.Role == chat.MessageRoleUser {
				userMessages = append(userMessages, msg.Message.Content)
			}
		}

		go a.generateTitle(ctx, userMessages)
		return nil
	}

	// For remote runtime, title regeneration is not yet supported
	// (the server would need to implement this)
	slog.Debug("Title regeneration not available for remote runtime", "session_id", a.session.ID)
	return errors.New("title regeneration not available")
}

// liveNodeWritable returns true when the live session node represents a session
// that can accept new user messages. Finalized (closed/stopped/failed)
// sessions are read-only: the user can read the transcript but not send.
func liveNodeWritable(node runtime.LiveSessionNode) bool {
	switch node.Status {
	case "closed", "stopped", "failed":
		return false
	default:
		return true
	}
}
