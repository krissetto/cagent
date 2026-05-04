package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

type ResumeType string

// ElicitationResult represents the result of an elicitation request
type ElicitationResult struct {
	Action  tools.ElicitationAction
	Content map[string]any // The submitted form data (only present when action is "accept")
}

// ElicitationError represents an error from a declined/cancelled elicitation
type ElicitationError struct {
	Action  string
	Message string
}

func (e *ElicitationError) Error() string {
	return fmt.Sprintf("elicitation %s: %s", e.Action, e.Message)
}

const (
	ResumeTypeApprove        ResumeType = "approve"
	ResumeTypeApproveSession ResumeType = "approve-session"
	ResumeTypeApproveTool    ResumeType = "approve-tool"
	ResumeTypeReject         ResumeType = "reject"
)

// ResumeRequest carries the user's confirmation decision along with an optional
// reason (used when rejecting a tool call to help the model understand why).
type ResumeRequest struct {
	Type     ResumeType
	Reason   string // Optional; primarily used with ResumeTypeReject
	ToolName string // Optional; used with ResumeTypeApproveTool to specify which tool to always allow
}

// ResumeApprove creates a ResumeRequest to approve a single tool call.
func ResumeApprove() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApprove}
}

// ResumeApproveSession creates a ResumeRequest to approve all tool calls for the session.
func ResumeApproveSession() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveSession}
}

// ResumeApproveTool creates a ResumeRequest to always approve a specific tool for the session.
func ResumeApproveTool(toolName string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveTool, ToolName: toolName}
}

// ResumeReject creates a ResumeRequest to reject a tool call with an optional reason.
func ResumeReject(reason string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeReject, Reason: reason}
}

// toolHandlerFunc is a function type for handling tool calls.
// The sr parameter is the invoking [sessionRunner], which gives handlers
// access to the correct per-session state (e.g. for launching nested
// sub-sessions that share the invoking runner's coordination channels
// rather than the root's).
type toolHandlerFunc func(ctx context.Context, sr *sessionRunner, sess *session.Session, toolCall tools.ToolCall, events chan Event) (*tools.ToolCallResult, error)

// ElicitationRequestHandler is a function type for handling elicitation requests
type ElicitationRequestHandler func(ctx context.Context, message string, schema map[string]any) (map[string]any, error)

// Runtime defines the contract for runtime execution
type Runtime interface {
	// CurrentAgentInfo returns information about the currently active agent
	CurrentAgentInfo(ctx context.Context) CurrentAgentInfo
	// CurrentAgentName returns the name of the currently active agent
	CurrentAgentName() string
	// SetCurrentAgent sets the currently active agent for subsequent user messages
	SetCurrentAgent(agentName string) error
	// CurrentAgentTools returns the tools for the active agent
	CurrentAgentTools(ctx context.Context) ([]tools.Tool, error)
	// EmitStartupInfo emits initial agent, team, and toolset information for immediate display.
	// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted
	// so the UI can display context usage percentage on session restore.
	EmitStartupInfo(ctx context.Context, sess *session.Session, events chan Event)
	// ResetStartupInfo resets the startup info emission flag, allowing re-emission
	ResetStartupInfo()
	// RunStream starts the agent's interaction loop and returns a channel of events
	RunStream(ctx context.Context, sess *session.Session) <-chan Event
	// Run starts the agent's interaction loop and returns the final messages
	Run(ctx context.Context, sess *session.Session) ([]session.Message, error)
	// Resume allows resuming execution after user confirmation.
	// The ResumeRequest carries the decision type and an optional reason (for rejections).
	Resume(ctx context.Context, req ResumeRequest)
	// ResumeElicitation sends an elicitation response back to a waiting elicitation request
	ResumeElicitation(_ context.Context, action tools.ElicitationAction, content map[string]any) error
	// SessionStore returns the session store for browsing/loading past sessions.
	// Returns nil if no persistent session store is configured.
	SessionStore() session.Store

	// Summarize generates a summary for the session
	Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan Event)

	// PermissionsInfo returns the team-level permission patterns (allow/ask/deny).
	// Returns nil if no permissions are configured.
	PermissionsInfo() *PermissionsInfo

	// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if skills are not enabled.
	CurrentAgentSkillsToolset() *builtin.SkillsToolset

	// CurrentMCPPrompts returns MCP prompts available from the current agent's toolsets.
	// Returns an empty map if no MCP prompts are available.
	CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo

	// ExecuteMCPPrompt executes a named MCP prompt with the given arguments.
	ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error)

	// UpdateSessionTitle persists a new title for the current session.
	UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error

	// TitleGenerator returns a generator for automatic session titles, or nil
	// if the runtime does not support local title generation (e.g. remote runtimes).
	TitleGenerator() *sessiontitle.Generator

	// Steer enqueues a user message for urgent mid-turn injection into the
	// running agent loop. Returns an error if the queue is full or steering
	// is not available.
	Steer(msg QueuedMessage) error
	// FollowUp enqueues a message for end-of-turn processing. Each follow-up
	// gets a full undivided agent turn. Returns an error if the queue is full.
	FollowUp(msg QueuedMessage) error

	// Close releases resources held by the runtime (e.g., session store connections).
	Close() error
}

// PermissionsInfo contains the allow, ask, and deny patterns for tool permissions.
type PermissionsInfo struct {
	Allow []string
	Ask   []string
	Deny  []string
}

type CurrentAgentInfo struct {
	Name        string
	Description string
	Commands    types.Commands
}

type ModelStore interface {
	GetModel(ctx context.Context, modelID string) (*modelsdev.Model, error)
	GetDatabase(ctx context.Context) (*modelsdev.Database, error)
}

// ToolsChangeSubscriber is implemented by runtimes that can notify when
// toolsets report a change in their tool list (e.g. after an MCP
// ToolListChanged notification). The provided callback is invoked
// outside of any RunStream, so the UI can update the tool count
// immediately.
type ToolsChangeSubscriber interface {
	OnToolsChanged(handler func(Event))
}

// SessionObserverSubscriber is implemented by runtimes that can fan out
// live session-scoped events to multiple observers. This is the core
// observability surface required for attaching to live subagent sessions.
type SessionObserverSubscriber interface {
	SubscribeSession(ctx context.Context, sessionID string, buffer int) *Subscription
}

// SessionTreeProvider is implemented by runtimes that can expose the
// live agent/subagent tree and control descendant sessions directly.
type SessionTreeProvider interface {
	LiveSessionTree(rootSessionID string) []LiveSessionNode
	LiveSessionNode(sessionID string) (LiveSessionNode, bool)
	SteerSessionByID(sessionID string, msg QueuedMessage) error
	FollowUpSessionByID(sessionID string, msg QueuedMessage) error
	InterruptSessionByID(sessionID string) error
	CloseSessionByID(sessionID string) error
	StopSessionByID(sessionID string) error
}

// SessionStartupEmitter is an optional runtime capability that emits a
// fresh batch of startup events (AgentInfo, TeamInfo, ToolsetInfo,
// TokenUsage) scoped to a specific session and agent.
//
// Unlike [Runtime.EmitStartupInfo], this variant does not carry a
// "startup already emitted" guard and does not depend on the runtime's
// currently-selected agent. It is used by the TUI when seeding an
// attached child-session tab so the sidebar can show the child agent's
// real model/description/tool count/token usage immediately rather than a
// placeholder. Runtimes that cannot resolve a specific agent by name
// (today: the remote runtime) simply omit this interface; the caller is
// expected to fall back to a minimal seed.
type SessionStartupEmitter interface {
	EmitSessionStartupInfo(ctx context.Context, sess *session.Session, agentName string, events chan Event)
}

// LiveSessionProvider exposes the in-memory [*session.Session] for a
// descendant subagent. The TUI's attached-tab flow uses this so it can back
// a tab's [*app.App] with the real child session pointer and render live
// transcript updates without copying state. Only local runtimes can satisfy
// this because it hands out a direct pointer.
type LiveSessionProvider interface {
	LiveSession(sessionID string) (*session.Session, bool)
}

// runtimeCore holds the runtime-wide services that are safe to share
// across every live session managed by a single runtime instance: the
// team, tool/session/model stores, the subagent manager, the event bus,
// and other immutable configuration. It exists to make the per-session
// split painless: a per-session runner can hold a *runtimeCore directly
// without needing access to LocalRuntime's per-session coordination state.
//
// Today runtimeCore is consumed by [LocalRuntime] (which embeds it as an
// anonymous pointer field) and by internal [sessionRunner] instances,
// which pair the shared services here with fresh per-session state.
//
// Note: toolMap intentionally does not live here. Each [sessionRunner]
// (root or child) builds its own per-session toolMap during construction
// because handler registration still closes over receiver methods on
// the root runtime. See [registerDefaultToolsInto].
//
// bgAgents lives on [LocalRuntime] (not here) so its lifecycle hooks
// (StopAll on Close) have a stable owner; child runners construct their
// own bgAgents handler isolated from the root.
type runtimeCore struct {
	team        *team.Team
	tracer      trace.Tracer
	modelsStore ModelStore

	sessionCompaction bool
	managedOAuth      bool

	// retryOnRateLimit enables retry-with-backoff for HTTP 429 (rate limit) errors
	// when no fallback models are configured. When false (default), 429 errors are
	// treated as non-retryable and immediately fail or skip to the next model.
	// Library consumers can enable this via WithRetryOnRateLimit().
	retryOnRateLimit bool

	sessionStore session.Store

	workingDir string   // Working directory for hooks execution
	env        []string // Environment variables for hooks execution

	modelSwitcherCfg *ModelSwitcherConfig

	// fallbackCooldowns tracks per-agent cooldown state for sticky fallback behavior.
	// Cooldowns are runtime-wide so a fallback decision made by one session is
	// honoured by every other session sharing the same runtime core.
	fallbackCooldowns    map[string]*fallbackCooldownState
	fallbackCooldownsMux sync.RWMutex

	// subagents owns the runtime-managed subagent lifecycle, envelope
	// routing, and parent wakeup queues. It is shared because parent and
	// child sessions must observe the same tree.
	subagents *subagent.Manager

	// eventBus fans out every event emitted by any session this runtime
	// owns (root + subagents) to registered observers. Sharing the same
	// bus is what allows attaching to a child session from any layer.
	eventBus *EventBus

	// liveSessions tracks every session (root and subagent) whose engine
	// is currently running. The registry is the canonical source for
	// [LocalRuntime.LiveSessionTree] and [LocalRuntime.LiveSessionNode];
	// engines self-register in [sessionEngine.run] and unregister on exit.
	liveSessions *liveSessionRegistry

	// recorder, when non-nil, is the SessionRecorder registered as a global
	// EventBus observer in [New]. The runtime owns its lifecycle so that
	// [LocalRuntime.Close] can drain in-flight async store writes before
	// returning. Nil when New() was not used.
	recorder *SessionRecorder
}

// LocalRuntime manages the execution of agents.
//
// NOTE — shared vs session-local state:
//
// LocalRuntime carries both runtime-wide shared services (via the embedded
// *runtimeCore) and the root session's mutable coordination state (via the
// embedded *sessionState). Root execution uses those fields directly through
// a root [sessionRunner]. Child sessions are driven through a child
// [sessionRunner] that pairs this runtime's shared *runtimeCore with a fresh
// *sessionState, without any child LocalRuntime.
//
// The comment on each field below indicates whether it is runtime-wide
// (shared) or root-session-only.
type LocalRuntime struct {
	// Embedded shared services. Field accesses like r.team or r.tracer
	// resolve here transparently through Go's promoted-field rules.
	*runtimeCore

	// Embedded root-session coordination state. Field accesses like
	// r.resumeChan, r.inbox, and r.currentAgent resolve here through
	// promoted-field rules. Child sessions do not get a child
	// LocalRuntime anymore; they run through a [sessionRunner] with its
	// own distinct *sessionState while sharing the same *runtimeCore.
	*sessionState

	// bgAgents holds the root background-agent dispatcher. Child
	// sessionRunners construct their own dispatcher during runner
	// creation. The root dispatcher lives on LocalRuntime so its
	// StopAll lifecycle hook has a stable owner accessible from
	// [LocalRuntime.Close].
	bgAgents *agenttool.Handler

	// onToolsChanged is called when an MCP toolset reports a tool list change.
	onToolsChanged func(Event)

	// subagentIdleAutoFinalize controls the optional idle sweeper in the
	// subagent manager. Zero means disabled (the default). It is consumed
	// only during construction; the value beyond that lives on the manager.
	subagentIdleAutoFinalize time.Duration

	// customSubagentRunner, when non-nil, replaces LocalRuntime as the
	// [subagent.Runner] passed to [subagent.NewManager]. This is the
	// injection point for alternative child-loop backends.
	// Set via [WithSubagentRunner]; consumed once during construction.
	customSubagentRunner subagent.Runner
}

type Opt func(*LocalRuntime)

// WithSubagentIdleAutoFinalize enables auto-finalizing stale idle subagents
// after the given timeout. A zero or negative timeout disables the feature.
//
// This is intentionally opt-in. Leaving the timeout unset preserves the
// historic behaviour where background subagents remain alive until the agent
// explicitly finalizes or stops them.
func WithSubagentIdleAutoFinalize(timeout time.Duration) Opt {
	return func(r *LocalRuntime) {
		if timeout > 0 {
			r.subagentIdleAutoFinalize = timeout
		}
	}
}

// WithSubagentRunner overrides the default child-loop implementation used by
// the [subagent.Manager] for new subagents. By default [LocalRuntime] uses
// itself (which drives child sessions through the same [sessionEngine] as
// root sessions). Embedders can supply a custom [subagent.Runner] to route
// child loops to an alternative backend — for example a remote runner.
//
// The runner is wired during [NewLocalRuntime] construction. It cannot be
// changed after the runtime is created.
func WithSubagentRunner(runner subagent.Runner) Opt {
	return func(r *LocalRuntime) {
		r.customSubagentRunner = runner
	}
}

func WithCurrentAgent(agentName string) Opt {
	return func(r *LocalRuntime) {
		r.currentAgent = agentName
	}
}

func WithManagedOAuth(managed bool) Opt {
	return func(r *LocalRuntime) {
		r.managedOAuth = managed
	}
}

// WithTracer sets a custom OpenTelemetry tracer; if not provided, tracing is disabled (no-op).
func WithTracer(t trace.Tracer) Opt {
	return func(r *LocalRuntime) {
		r.tracer = t
	}
}

// WithSteerQueue sets a custom MessageQueue for mid-turn message injection.
// If not provided, an in-memory buffered queue is used.
func WithSteerQueue(q MessageQueue) Opt {
	return func(r *LocalRuntime) {
		if r.sessionState == nil {
			r.sessionState = newSessionState("")
		}
		r.steer = q
	}
}

// WithFollowUpQueue sets a custom MessageQueue for end-of-turn follow-up
// messages. If not provided, an in-memory buffered queue is used.
func WithFollowUpQueue(q MessageQueue) Opt {
	return func(r *LocalRuntime) {
		if r.sessionState == nil {
			r.sessionState = newSessionState("")
		}
		r.followUp = q
	}
}

func WithSessionCompaction(sessionCompaction bool) Opt {
	return func(r *LocalRuntime) {
		r.sessionCompaction = sessionCompaction
	}
}

func WithModelStore(store ModelStore) Opt {
	return func(r *LocalRuntime) {
		r.modelsStore = store
	}
}

func WithSessionStore(store session.Store) Opt {
	return func(r *LocalRuntime) {
		r.sessionStore = store
	}
}

// WithWorkingDir sets the working directory for hooks execution
func WithWorkingDir(dir string) Opt {
	return func(r *LocalRuntime) {
		r.workingDir = dir
	}
}

// WithEnv sets the environment variables for hooks execution
func WithEnv(env []string) Opt {
	return func(r *LocalRuntime) {
		r.env = env
	}
}

// WithRetryOnRateLimit enables automatic retry with backoff for HTTP 429 (rate limit)
// errors when no fallback models are available. When enabled, the runtime will honor
// the Retry-After header from the provider's response to determine wait time before
// retrying, falling back to exponential backoff if the header is absent.
//
// This is off by default. It is intended for library consumers that run agents
// programmatically and prefer to wait for rate limits to clear rather than fail
// immediately.
//
// When fallback models are configured, 429 errors always skip to the next model
// regardless of this setting.
func WithRetryOnRateLimit() Opt {
	return func(r *LocalRuntime) {
		r.retryOnRateLimit = true
	}
}

// NewLocalRuntime creates a new LocalRuntime without persistence.
// This is useful for testing or when persistence is handled externally.
func NewLocalRuntime(agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	defaultAgent, err := agents.DefaultAgent()
	if err != nil {
		return nil, err
	}

	r := &LocalRuntime{
		runtimeCore: &runtimeCore{
			team:              agents,
			sessionCompaction: true,
			managedOAuth:      true,
			sessionStore:      session.NewInMemorySessionStore(),
			fallbackCooldowns: make(map[string]*fallbackCooldownState),
		},
		sessionState: newSessionState(defaultAgent.Name()),
	}

	for _, opt := range opts {
		opt(r)
	}

	r.bgAgents = agenttool.NewHandler(r)
	runner := subagent.Runner(r)
	if r.customSubagentRunner != nil {
		runner = r.customSubagentRunner
	}
	if r.subagentIdleAutoFinalize > 0 {
		r.subagents = subagent.NewManager(runner, subagent.WithIdleAutoFinalize(r.subagentIdleAutoFinalize))
	} else {
		r.subagents = subagent.NewManager(runner)
	}
	r.subagents.AddEnvelopePublishedListener(func(env subagent.Envelope) {
		r.publishTreeChangeFromChild(env.SubAgentID, env.ParentSessionID)
	})
	r.subagents.AddChildRegisteredListener(func(h *subagent.Handle) {
		r.publishTreeChangeFromChild(h.ID(), h.ParentSessionID())
	})
	r.eventBus = NewEventBus()
	r.liveSessions = newLiveSessionRegistry()

	if r.modelsStore == nil {
		modelsStore, err := modelsdev.NewStore()
		if err != nil {
			return nil, err
		}
		r.modelsStore = modelsStore
	}

	// Validate that the current agent exists and has a model
	// (currentAgent might have been changed by options)
	defaultAgent, err = r.team.Agent(r.currentAgent)
	if err != nil {
		return nil, err
	}

	if defaultAgent.Model() == nil {
		return nil, fmt.Errorf("agent %s has no valid model", defaultAgent.Name())
	}

	slog.Debug("Creating new runtime", "agent", r.currentAgent, "available_agents", agents.Size())

	return r, nil
}

func (r *LocalRuntime) CurrentAgentName() string {
	r.currentAgentMu.RLock()
	defer r.currentAgentMu.RUnlock()
	return r.currentAgent
}

func (r *LocalRuntime) setCurrentAgent(name string) {
	r.currentAgentMu.Lock()
	defer r.currentAgentMu.Unlock()
	r.currentAgent = name
}

func (r *LocalRuntime) CurrentAgentInfo(context.Context) CurrentAgentInfo {
	currentAgent := r.CurrentAgent()

	return CurrentAgentInfo{
		Name:        currentAgent.Name(),
		Description: currentAgent.Description(),
		Commands:    currentAgent.Commands(),
	}
}

func (r *LocalRuntime) SetCurrentAgent(agentName string) error {
	// Validate that the agent exists in the team
	if _, err := r.team.Agent(agentName); err != nil {
		return err
	}
	r.setCurrentAgent(agentName)
	slog.Debug("Switched current agent", "agent", agentName)
	return nil
}

func (r *LocalRuntime) CurrentAgentCommands(context.Context) types.Commands {
	return r.CurrentAgent().Commands()
}

// CurrentAgentTools returns the tools available to the current agent.
// This starts the toolsets if needed and returns all available tools.
func (r *LocalRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	a := r.CurrentAgent()
	return a.Tools(ctx)
}

// CurrentMCPPrompts returns the available MCP prompts from all active MCP toolsets
// for the current agent. It discovers prompts by calling ListPrompts on each MCP toolset
// and aggregates the results into a map keyed by prompt name.
func (r *LocalRuntime) CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo {
	prompts := make(map[string]mcptools.PromptInfo)

	// Get the current agent to access its toolsets
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		slog.Warn("No current agent available for MCP prompt discovery")
		return prompts
	}

	// Iterate through all toolsets of the current agent
	for _, toolset := range currentAgent.ToolSets() {
		if mcpToolset, ok := tools.As[*mcptools.Toolset](toolset); ok {
			slog.Debug("Found MCP toolset", "toolset", mcpToolset)
			// Discover prompts from this MCP toolset
			mcpPrompts := r.discoverMCPPrompts(ctx, mcpToolset)

			// Merge prompts into the result map
			// If there are name conflicts, the later toolset's prompt will override
			maps.Copy(prompts, mcpPrompts)
		} else {
			slog.Debug("Toolset is not an MCP toolset", "type", fmt.Sprintf("%T", toolset))
		}
	}

	slog.Debug("Discovered MCP prompts", "agent", currentAgent.Name(), "prompt_count", len(prompts))
	return prompts
}

// discoverMCPPrompts queries an MCP toolset for available prompts and converts them
// to PromptInfo structures. This method handles the MCP protocol communication
// and gracefully handles any errors during prompt discovery.
func (r *LocalRuntime) discoverMCPPrompts(ctx context.Context, toolset *mcptools.Toolset) map[string]mcptools.PromptInfo {
	mcpPrompts, err := toolset.ListPrompts(ctx)
	if err != nil {
		slog.Warn("Failed to list MCP prompts from toolset", "error", err)
		return nil
	}

	prompts := make(map[string]mcptools.PromptInfo, len(mcpPrompts))
	for _, mcpPrompt := range mcpPrompts {
		promptInfo := mcptools.PromptInfo{
			Name:        mcpPrompt.Name,
			Description: mcpPrompt.Description,
			Arguments:   make([]mcptools.PromptArgument, 0, len(mcpPrompt.Arguments)),
		}

		for _, arg := range mcpPrompt.Arguments {
			promptInfo.Arguments = append(promptInfo.Arguments, mcptools.PromptArgument{
				Name:        arg.Name,
				Description: arg.Description,
				Required:    arg.Required,
			})
		}

		prompts[mcpPrompt.Name] = promptInfo
		slog.Debug("Discovered MCP prompt", "name", mcpPrompt.Name, "args_count", len(promptInfo.Arguments))
	}

	return prompts
}

// currentRootAgent returns the runtime's currently selected root-session
// agent. This is the agent used for new user work entering the root session.
// It is intentionally distinct from [resolveSessionAgent], which answers a
// different question: which agent identity is pinned to *this* session.
func (r *LocalRuntime) currentRootAgent() *agent.Agent {
	// We validated already that the agent exists.
	current, _ := r.team.Agent(r.CurrentAgentName())
	return current
}

// CurrentAgent returns the runtime's currently selected root-session agent.
// Session-scoped execution should prefer [resolveSessionAgent] so pinned
// child/background sessions do not accidentally observe the mutable global
// currentAgent selection.
func (r *LocalRuntime) CurrentAgent() *agent.Agent {
	return r.currentRootAgent()
}

// resolveSessionAgent returns the agent identity pinned to the given session.
// If sess.AgentName is set, that agent wins even when the runtime's mutable
// currentAgent points somewhere else (for example while an attached child tab
// is open or a background session is running concurrently). When the session
// is not pinned, we fall back to the root runtime's current agent selection.
func (r *LocalRuntime) resolveSessionAgent(sess *session.Session) *agent.Agent {
	if sess.AgentName != "" {
		if a, err := r.team.Agent(sess.AgentName); err == nil {
			return a
		}
	}
	return r.CurrentAgent()
}

// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if not enabled.
func (r *LocalRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if st, ok := tools.As[*builtin.SkillsToolset](ts); ok {
			return st
		}
	}
	return nil
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content.
func (r *LocalRuntime) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		return "", errors.New("no current agent available")
	}

	for _, toolset := range currentAgent.ToolSets() {
		mcpToolset, ok := tools.As[*mcptools.Toolset](toolset)
		if !ok {
			continue
		}

		result, err := mcpToolset.GetPrompt(ctx, promptName, arguments)
		if err != nil {
			// If error is "prompt not found", continue to next toolset
			if err.Error() == "prompt not found" {
				continue
			}
			return "", fmt.Errorf("error executing prompt '%s': %w", promptName, err)
		}

		// Convert the MCP result to a string format
		if len(result.Messages) == 0 {
			return "No content returned from MCP prompt", nil
		}

		var content strings.Builder
		for i, message := range result.Messages {
			if i > 0 {
				content.WriteString("\n\n")
			}
			if textContent, ok := message.Content.(*mcp.TextContent); ok {
				content.WriteString(textContent.Text)
			} else {
				fmt.Fprintf(&content, "[Non-text content: %T]", message.Content)
			}
		}
		return content.String(), nil
	}

	return "", fmt.Errorf("MCP prompt '%s' not found in any active toolset", promptName)
}

// TitleGenerator returns a title generator for automatic session title generation.
func (r *LocalRuntime) TitleGenerator() *sessiontitle.Generator {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	model := a.Model()
	if model == nil {
		return nil
	}
	return sessiontitle.New(model, a.FallbackModels()...)
}

// getHooksExecutor creates a hooks executor for the given agent
func (r *LocalRuntime) getHooksExecutor(a *agent.Agent) *hooks.Executor {
	hooksCfg := hooks.FromConfig(a.Hooks())
	if hooksCfg == nil || hooksCfg.IsEmpty() {
		return nil
	}
	return hooks.NewExecutor(hooksCfg, r.workingDir, r.env)
}

// executeSessionStartHooks executes session start hooks for the given agent.
// It logs the hook output as additional context and emits warnings for system messages.
func (r *LocalRuntime) executeSessionStartHooks(ctx context.Context, sess *session.Session, a *agent.Agent, events chan Event) {
	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasSessionStartHooks() {
		return
	}

	slog.Debug("Executing session start hooks", "agent", a.Name(), "session_id", sess.ID)
	input := &hooks.Input{
		SessionID: sess.ID,
		Cwd:       r.workingDir,
		Source:    "startup",
	}

	result, err := hooksExec.ExecuteSessionStart(ctx, input)
	if err != nil {
		slog.Warn("Session start hook execution failed", "agent", a.Name(), "error", err)
		return
	}

	if result.SystemMessage != "" {
		events <- Warning(result.SystemMessage, a.Name())
	}
	if result.AdditionalContext != "" {
		slog.Debug("Session start hook provided additional context", "context", result.AdditionalContext)
		sess.AddMessage(session.SystemMessage(result.AdditionalContext))
	}
}

// executeSessionEndHooks executes session end hooks for the given agent.
func (r *LocalRuntime) executeSessionEndHooks(ctx context.Context, sess *session.Session, a *agent.Agent) {
	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasSessionEndHooks() {
		return
	}

	slog.Debug("Executing session end hooks", "agent", a.Name(), "session_id", sess.ID)
	input := &hooks.Input{
		SessionID: sess.ID,
		Cwd:       r.workingDir,
		Reason:    "stream_ended",
	}

	_, err := hooksExec.ExecuteSessionEnd(ctx, input)
	if err != nil {
		slog.Error("Session end hook execution failed", "agent", a.Name(), "error", err)
	}
}

// executeStopHooks executes stop hooks when the model finishes responding.
// The stop hook receives the model's final response content.
func (r *LocalRuntime) executeStopHooks(ctx context.Context, sess *session.Session, a *agent.Agent, responseContent string, events chan Event) {
	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasStopHooks() {
		return
	}

	slog.Debug("Executing stop hooks", "agent", a.Name(), "session_id", sess.ID)
	input := &hooks.Input{
		SessionID:    sess.ID,
		Cwd:          r.workingDir,
		StopResponse: responseContent,
	}

	result, err := hooksExec.ExecuteStop(ctx, input)
	if err != nil {
		slog.Warn("Stop hook execution failed", "agent", a.Name(), "error", err)
		return
	}

	if result.SystemMessage != "" {
		events <- Warning(result.SystemMessage, a.Name())
	}
}

// executeNotificationHooks executes notification hooks when the agent emits a user-facing
// notification (e.g., errors or warnings). Hook output is logged but does not affect the
// notification itself. Individual hooks are subject to their configured timeout.
func (r *LocalRuntime) executeNotificationHooks(ctx context.Context, a *agent.Agent, sessionID, level, message string) {
	if a == nil {
		return
	}

	if level != "error" && level != "warning" {
		slog.Error("Invalid notification level", "level", level, "expected", "error|warning")
		return
	}

	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasNotificationHooks() {
		return
	}

	slog.Debug("Executing notification hooks", "level", level, "session_id", sessionID)
	input := &hooks.Input{
		SessionID:           sessionID,
		Cwd:                 r.workingDir,
		NotificationLevel:   level,
		NotificationMessage: message,
	}

	_, err := hooksExec.ExecuteNotification(ctx, input)
	if err != nil {
		slog.Warn("Notification hook execution failed", "error", err)
	}
}

// executeOnUserInputHooks executes on-user-input hooks for the given agent.
// The caller is responsible for passing the session-resolved agent so that
// child sessions use the correct agent identity instead of the runtime's
// mutable global CurrentAgentName().
func (r *LocalRuntime) executeOnUserInputHooks(ctx context.Context, a *agent.Agent, sessionID, logContext string) {
	if a == nil {
		return
	}

	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasOnUserInputHooks() {
		return
	}

	slog.Debug("Executing on-user-input hooks", "context", logContext)
	input := &hooks.Input{
		SessionID: sessionID,
		Cwd:       r.workingDir,
	}

	result, err := hooksExec.ExecuteOnUserInput(ctx, input)
	if err != nil {
		slog.Warn("On-user-input hook execution failed", "error", err)
	} else {
		slog.Debug("On-user-input hooks executed successfully")
	}
	_ = result // Hook result not used
}

// getAgentModelID returns the model ID for an agent, or empty string if no model is set.
func getAgentModelID(a *agent.Agent) string {
	if model := a.Model(); model != nil {
		return model.ID()
	}
	return ""
}

// getEffectiveModelID returns the currently active model ID for an agent, accounting
// for any active fallback cooldown. During a cooldown period, this returns the fallback
// model ID instead of the configured primary model, so the UI reflects the actual model in use.
func (r *LocalRuntime) getEffectiveModelID(a *agent.Agent) string {
	cooldownState := r.getCooldownState(a.Name())
	if cooldownState != nil {
		fallbacks := a.FallbackModels()
		if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
			return fallbacks[cooldownState.fallbackIndex].ID()
		}
	}
	return getAgentModelID(a)
}

// agentDetailsFromTeam converts team agent info to AgentDetails for events.
// It accounts for active fallback cooldowns, returning the effective model
// instead of the configured model when a fallback is in effect.
func (r *LocalRuntime) agentDetailsFromTeam() []AgentDetails {
	agentsInfo := r.team.AgentsInfo()
	details := make([]AgentDetails, len(agentsInfo))
	for i, info := range agentsInfo {
		providerName := info.Provider
		modelName := info.Model

		// Check if this agent has an active fallback cooldown
		cooldownState := r.getCooldownState(info.Name)
		if cooldownState != nil {
			// Get the agent to access fallback models
			if a, err := r.team.Agent(info.Name); err == nil && a != nil {
				fallbacks := a.FallbackModels()
				if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
					fb := fallbacks[cooldownState.fallbackIndex]
					// Parse provider/model from the fallback model ID
					modelID := fb.ID()
					if p, m, found := strings.Cut(modelID, "/"); found {
						providerName = p
						modelName = m
					} else {
						modelName = modelID
					}
				}
			}
		}

		details[i] = AgentDetails{
			Name:        info.Name,
			Description: info.Description,
			Provider:    providerName,
			Model:       modelName,
			Commands:    info.Commands,
		}
	}
	return details
}

// SessionStore returns the session store for browsing/loading past sessions.
func (r *LocalRuntime) SessionStore() session.Store {
	return r.sessionStore
}

// Close releases resources held by the runtime, including the session store.
func (r *LocalRuntime) Close() error {
	r.bgAgents.StopAll()
	if r.subagents != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.subagents.StopAll(ctx)
		r.subagents.Shutdown()
	}
	// Drain any in-flight recorder writes before closing the store so we do
	// not race a worker goroutine performing an AddMessage against a closed
	// SQLite handle.
	if r.recorder != nil {
		r.recorder.Close()
	}
	if r.sessionStore != nil {
		return r.sessionStore.Close()
	}
	return nil
}

// UpdateSessionTitle persists the session title via the session store.
//
// The in-memory update goes through [session.Session.SetTitle] because child
// sessions (subagents) run in their own goroutines and may have concurrent
// readers — e.g. the TUI snapshotting `sess.Title` when opening an attached
// tab. A raw `sess.Title = title` is a data race in that path even though the
// store persistence itself is serialised.
//
// Persistence uses the narrow [session.Store.UpdateSessionTitle] rather than
// the full [session.Store.UpdateSession] to avoid racing with the child-loop
// goroutine that concurrently writes InputTokens/OutputTokens on the same
// session object.
func (r *LocalRuntime) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	sess.SetTitle(title)
	if r.sessionStore != nil {
		return r.sessionStore.UpdateSessionTitle(ctx, sess.ID, title)
	}
	return nil
}

// PermissionsInfo returns the team-level permission patterns.
// Returns nil if no permissions are configured.
func (r *LocalRuntime) PermissionsInfo() *PermissionsInfo {
	permChecker := r.team.Permissions()
	if permChecker == nil || permChecker.IsEmpty() {
		return nil
	}
	return &PermissionsInfo{
		Allow: permChecker.AllowPatterns(),
		Ask:   permChecker.AskPatterns(),
		Deny:  permChecker.DenyPatterns(),
	}
}

// ResetStartupInfo resets the startup info emission flag.
// This should be called when replacing a session to allow re-emission of
// agent, team, and toolset info to the UI.
func (r *LocalRuntime) ResetStartupInfo() {
	r.startupInfoEmitted = false
}

// OnToolsChanged registers a handler that is called when an MCP toolset
// reports a tool list change outside of a RunStream. This allows the UI
// to update the tool count immediately.
func (r *LocalRuntime) OnToolsChanged(handler func(Event)) {
	r.onToolsChanged = handler

	for _, name := range r.team.AgentNames() {
		a, err := r.team.Agent(name)
		if err != nil {
			continue
		}
		for _, ts := range a.ToolSets() {
			if n, ok := tools.As[tools.ChangeNotifier](ts); ok {
				n.SetToolsChangedHandler(r.emitToolsChanged)
			}
		}
	}
}

// emitToolsChanged is the callback registered on MCP toolsets. It re-reads
// the current agent's full tool list and pushes a ToolsetInfo event.
func (r *LocalRuntime) emitToolsChanged() {
	if r.onToolsChanged == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := r.CurrentAgent()
	agentTools, err := a.StartedTools(ctx)
	if err != nil {
		return
	}
	r.onToolsChanged(ToolsetInfo(len(agentTools), false, r.CurrentAgentName()))
}

// EmitStartupInfo emits initial agent, team, and toolset information for immediate sidebar display.
// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted so that the
// sidebar can display context usage percentage on session restore.
func (r *LocalRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events chan Event) {
	// Prevent duplicate emissions
	if r.startupInfoEmitted {
		return
	}
	r.startupInfoEmitted = true

	r.emitStartupInfoForAgent(ctx, sess, r.CurrentAgent(), events)
}

// EmitSessionStartupInfo emits startup metadata scoped to the provided
// session/agent pair, without consulting or mutating the runtime's global
// current-agent selection. It shares the implementation path
// [emitStartupInfoForAgent] with [EmitStartupInfo]; the only difference is
// that it has no once-guard and resolves the agent by name instead of via
// the runtime's mutable currentAgent field.
//
// Primary use: attached child-session tabs that need the child agent's real
// model/description/tool count immediately.
func (r *LocalRuntime) EmitSessionStartupInfo(ctx context.Context, sess *session.Session, agentName string, events chan Event) {
	a, err := r.team.Agent(agentName)
	if err != nil || a == nil {
		return
	}
	r.emitStartupInfoForAgent(ctx, sess, a, events)
}

// emitStartupInfoForAgent contains the actual startup-info emission logic shared
// by the normal root-tab path and the attached child-session path.
func (r *LocalRuntime) emitStartupInfoForAgent(ctx context.Context, sess *session.Session, a *agent.Agent, events chan Event) {
	// Helper to send events with context check
	send := func(event Event) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Emit agent and team information immediately for fast sidebar display.
	// Use getEffectiveModelID to account for active fallback cooldowns.
	modelID := r.getEffectiveModelID(a)
	if !send(AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage())) {
		return
	}
	if !send(TeamInfo(r.agentDetailsFromTeam(), a.Name())) {
		return
	}

	// When restoring a session that already has token data, emit a
	// TokenUsageEvent so the sidebar can show the context usage percentage.
	// The context limit comes from the model definition (models.dev), which
	// is a model property — not persisted in the session.
	//
	// Use TotalCost (not OwnCost) because this is a restore/branch context:
	// sub-sessions won't emit their own events, so the parent must include
	// their costs.
	if sess != nil && (sess.InputTokens > 0 || sess.OutputTokens > 0) {
		var contextLimit int64
		if m, err := r.modelsStore.GetModel(ctx, modelID); err == nil && m != nil {
			contextLimit = int64(m.Limit.Context)
		}
		usage := SessionUsage(sess, contextLimit)
		usage.Cost = sess.TotalCost()

		// Reconstruct LastMessage from the session's last assistant message so
		// FinishReason (and other per-message fields) are available on restore.
		for _, item := range slices.Backward(sess.Messages) {
			if !item.IsMessage() || item.Message.Message.Role != chat.MessageRoleAssistant {
				continue
			}
			msg := item.Message.Message
			lm := &MessageUsage{Model: msg.Model, Cost: msg.Cost, FinishReason: msg.FinishReason}
			if msg.Usage != nil {
				lm.Usage = *msg.Usage
			}
			usage.LastMessage = lm
			break
		}

		if !send(NewTokenUsageEvent(sess.ID, a.Name(), usage)) {
			return
		}
	}

	// Emit agent warnings (if any) - these are quick.
	r.emitAgentWarnings(a, func(e Event) { send(e) })

	// Tool loading can be slow (MCP servers need to start).
	r.emitToolsProgressivelyForAgent(ctx, a, send)
}

func (r *LocalRuntime) emitToolsProgressivelyForAgent(ctx context.Context, a *agent.Agent, send func(Event) bool) {
	toolsets := a.ToolSets()
	totalToolsets := len(toolsets)

	// If no toolsets, emit final state immediately
	if totalToolsets == 0 {
		send(ToolsetInfo(0, false, a.Name()))
		return
	}

	// Emit initial loading state
	if !send(ToolsetInfo(0, true, a.Name())) {
		return
	}

	// Load tools from each toolset and emit progress
	var totalTools int
	for i, toolset := range toolsets {
		// Check context before potentially slow operations
		if ctx.Err() != nil {
			return
		}

		isLast := i == totalToolsets-1

		// Start the toolset if needed
		if startable, ok := toolset.(*tools.StartableToolSet); ok {
			if !startable.IsStarted() {
				if err := startable.Start(ctx); err != nil {
					slog.Warn("Toolset start failed; skipping", "agent", a.Name(), "toolset", fmt.Sprintf("%T", startable.ToolSet), "error", err)
					continue
				}
			}
		}

		// Get tools from this toolset
		ts, err := toolset.Tools(ctx)
		if err != nil {
			slog.Warn("Failed to get tools from toolset", "agent", a.Name(), "error", err)
			continue
		}

		totalTools += len(ts)

		// Emit progress update - still loading unless this is the last toolset
		if !send(ToolsetInfo(totalTools, !isLast, a.Name())) {
			return
		}
	}

	// Emit final state (not loading)
	send(ToolsetInfo(totalTools, false, a.Name()))
}

func (r *LocalRuntime) Resume(_ context.Context, req ResumeRequest) {
	slog.Debug("Resuming runtime", "agent", r.CurrentAgentName(), "type", req.Type, "reason", req.Reason)

	// Defensive validation:
	//
	// The runtime may be resumed by multiple entry points (API, CLI, TUI, tests).
	// Even if upstream layers perform validation, the runtime must never assume
	// the ResumeType is valid. Accepting invalid values here leads to confusing
	// downstream behavior where tool execution fails without a clear cause.
	if !IsValidResumeType(req.Type) {
		slog.Warn(
			"Invalid resume type received; ignoring resume request",
			"agent", r.CurrentAgentName(),
			"confirmation_type", req.Type,
			"valid_types", ValidResumeTypes(),
		)
		return
	}

	// Attempt to deliver the resume signal to the execution loop.
	//
	// The channel is non-blocking by design to avoid deadlocks if the runtime
	// is not currently waiting for a confirmation (e.g. already resumed,
	// canceled, or shutting down).
	select {
	case r.resumeChan <- req:
		slog.Debug("Resume signal sent", "agent", r.CurrentAgentName())
	default:
		slog.Debug(
			"Resume channel not ready; resume signal dropped",
			"agent", r.CurrentAgentName(),
			"confirmation_type", req.Type,
		)
	}
}

// ResumeElicitation sends an elicitation response back to a waiting elicitation request
func (r *LocalRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	slog.Debug("Resuming runtime with elicitation response", "agent", r.CurrentAgentName(), "action", action)

	result := ElicitationResult{
		Action:  action,
		Content: content,
	}

	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled while sending elicitation response")
		return ctx.Err()
	case r.sessionState.elicitationRequestCh <- result:
		slog.Debug("Elicitation response sent successfully", "action", action)
		return nil
	default:
		slog.Debug("Elicitation channel not ready")
		return errors.New("no elicitation request in progress")
	}
}

// Steer enqueues a user message for urgent mid-turn injection into the
// running agent loop. The message will be picked up after the current batch
// of tool calls finishes but before the loop checks whether to stop.
func (r *LocalRuntime) Steer(msg QueuedMessage) error {
	if !r.steer.Enqueue(context.Background(), msg) {
		return errors.New("steer queue full")
	}
	return nil
}

// FollowUp enqueues a message to be processed after the current agent turn
// finishes. Unlike Steer, follow-ups are popped one at a time and each gets
// a full undivided agent turn.
func (r *LocalRuntime) FollowUp(msg QueuedMessage) error {
	if !r.followUp.Enqueue(context.Background(), msg) {
		return errors.New("follow-up queue full")
	}
	return nil
}

// Run starts the agent's interaction loop

func (r *LocalRuntime) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if r.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return r.tracer.Start(ctx, name, opts...)
}

// Summarize generates a summary for the session based on the conversation history.
// The additionalPrompt parameter allows users to provide additional instructions
// for the summarization (e.g., "focus on code changes" or "include action items").
func (r *LocalRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan Event) {
	a := r.resolveSessionAgent(sess)
	r.doCompact(ctx, sess, a, additionalPrompt, events)

	// Emit a TokenUsageEvent so the sidebar immediately reflects the
	// compaction: tokens drop to the summary size, context % drops, and
	// cost increases by the summary generation cost.
	modelID := r.getEffectiveModelID(a)
	var contextLimit int64
	if m, err := r.modelsStore.GetModel(ctx, modelID); err == nil && m != nil {
		contextLimit = int64(m.Limit.Context)
	}
	events <- NewTokenUsageEvent(sess.ID, a.Name(), SessionUsage(sess, contextLimit))
}

// New creates a new runtime for an agent team with persistence enabled.
//
// The runtime automatically persists session changes to the configured store
// via a [SessionRecorder] registered as a global EventBus observer. Every
// event published on any session topic (root or subagent) is handled through
// a single, unified path — no per-session wrapper drains are needed.
//
// Initial session metadata for root sessions is persisted synchronously at
// the start of [sessionRunner.runStreamWithConfig] (for non-sub-sessions).
//
// Returns a [Runtime] interface backed by a [*LocalRuntime].
func New(agents *team.Team, opts ...Opt) (Runtime, error) {
	r, err := NewLocalRuntime(agents, opts...)
	if err != nil {
		return nil, err
	}

	// Register a SessionRecorder as a global event-bus observer so every
	// event published on any session topic is persisted through a single,
	// unified path — no per-session wrappers or child-subscription drains.
	recorder := NewSessionRecorder(r.sessionStore)
	r.recorder = recorder
	r.eventBus.AddGlobalObserver(func(sessionID string, ev Event) {
		recorder.Handle(sessionID, ev)
	})

	return r, nil
}
