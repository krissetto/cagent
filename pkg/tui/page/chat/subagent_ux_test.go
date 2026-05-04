package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appcore "github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type liveTreeOnlyRuntime struct {
	runtime.Runtime

	tree []runtime.LiveSessionNode
}

func (r *liveTreeOnlyRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}
func (r *liveTreeOnlyRuntime) CurrentAgentName() string     { return "root" }
func (r *liveTreeOnlyRuntime) SetCurrentAgent(string) error { return nil }
func (r *liveTreeOnlyRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (r *liveTreeOnlyRuntime) EmitStartupInfo(context.Context, *session.Session, chan runtime.Event) {
}
func (r *liveTreeOnlyRuntime) ResetStartupInfo() {}
func (r *liveTreeOnlyRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

func (r *liveTreeOnlyRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (r *liveTreeOnlyRuntime) Resume(context.Context, runtime.ResumeRequest) {}
func (r *liveTreeOnlyRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any) error {
	return nil
}
func (r *liveTreeOnlyRuntime) SessionStore() session.Store { return nil }
func (r *liveTreeOnlyRuntime) Summarize(context.Context, *session.Session, string, chan runtime.Event) {
}
func (r *liveTreeOnlyRuntime) PermissionsInfo() *runtime.PermissionsInfo         { return nil }
func (r *liveTreeOnlyRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset { return nil }
func (r *liveTreeOnlyRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}

func (r *liveTreeOnlyRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (r *liveTreeOnlyRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (r *liveTreeOnlyRuntime) TitleGenerator() *sessiontitle.Generator                 { return nil }
func (r *liveTreeOnlyRuntime) Steer(runtime.QueuedMessage) error                       { return nil }
func (r *liveTreeOnlyRuntime) FollowUp(runtime.QueuedMessage) error                    { return nil }
func (r *liveTreeOnlyRuntime) Close() error                                            { return nil }
func (r *liveTreeOnlyRuntime) LiveSessionTree(rootID string) []runtime.LiveSessionNode { return r.tree }
func (r *liveTreeOnlyRuntime) LiveSessionNode(string) (runtime.LiveSessionNode, bool) {
	return runtime.LiveSessionNode{}, false
}
func (r *liveTreeOnlyRuntime) SteerSessionByID(string, runtime.QueuedMessage) error    { return nil }
func (r *liveTreeOnlyRuntime) FollowUpSessionByID(string, runtime.QueuedMessage) error { return nil }
func (r *liveTreeOnlyRuntime) InterruptSessionByID(string) error                       { return nil }
func (r *liveTreeOnlyRuntime) CloseSessionByID(string) error                           { return nil }
func (r *liveTreeOnlyRuntime) StopSessionByID(string) error                            { return nil }

// TestHandleSubAgentUpdate_RefreshesSidebarTreeForNestedDescendants verifies
// the root tab sidebar refreshes from the live tree when a subagent event
// arrives, so nested descendants (grandchildren) appear immediately without
// needing the user to switch into a sub-session tab and back.
func TestHandleSubAgentUpdate_RefreshesSidebarTreeForNestedDescendants(t *testing.T) {
	t.Parallel()

	rootSess := session.New()
	ss := service.NewSessionState(rootSess)
	rt := &liveTreeOnlyRuntime{tree: []runtime.LiveSessionNode{
		{SessionID: rootSess.ID, RootSessionID: rootSess.ID, Kind: runtime.LiveSessionRoot, AgentName: "root"},
		{SessionID: "child-123456", ParentSessionID: rootSess.ID, RootSessionID: rootSess.ID, Kind: runtime.LiveSessionSubAgent, AgentName: "worker", Status: "waiting"},
		{SessionID: "grandchild-abcdef", ParentSessionID: "child-123456", RootSessionID: rootSess.ID, Kind: runtime.LiveSessionSubAgent, AgentName: "leaf", Status: "waiting"},
	}}
	app := appcore.New(t.Context(), rt, rootSess)
	p := &chatPage{
		app:          app,
		sidebar:      sidebar.New(ss),
		messages:     messages.New(ss),
		sessionState: ss,
	}
	cmd := p.SetSize(140, 20)
	_ = cmd

	// Before any event-driven refresh, the sidebar knows nothing about the
	// live descendant tree and therefore should not mention the nested leaf.
	assert.NotContains(t, p.sidebar.View(), "leaf")

	_, cmd = p.handleRuntimeEvent(&runtime.SubAgentUpdateEvent{
		SessionID: rootSess.ID,
		Envelope: subagent.Envelope{
			SubAgentID:      "child-123456",
			ParentSessionID: rootSess.ID,
			AgentName:       "worker",
			Kind:            subagent.UpdateKindTurnCompleted,
			Status:          subagent.StatusWaiting,
		},
	})
	require.NotNil(t, cmd)

	// In the real system, the manager's onEnvelopePublished hook fires
	// LiveSessionTreeChangedEvent to ancestor buses. Simulate that here.
	_, _ = p.handleRuntimeEvent(&runtime.LiveSessionTreeChangedEvent{
		Type:      "live_session_tree_changed",
		SessionID: rootSess.ID,
	})

	view := p.sidebar.View()
	assert.Contains(t, view, "worker", "direct child should remain visible in the parent sidebar")
	assert.Contains(t, view, "leaf", "nested live descendant should appear after subagent event refreshes the tree")
	assert.True(t, strings.Contains(view, "└ ") || strings.Contains(view, "├ "), "sidebar should render tree connectors for nested descendants")
}

// newTurnCompletedTestPage wires a chat page without a real *app.App. The
// TurnCompleted suppression path exercised below only touches the sidebar and
// messages components, so we can follow the same minimal-fixture pattern as
// attached_spinner_test.go without spinning up a full runtime.
func newTurnCompletedTestPage(t *testing.T) *chatPage {
	t.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	p := &chatPage{
		sidebar:      sidebar.New(ss),
		messages:     messages.New(ss),
		sessionState: ss,
	}
	p.SetSize(140, 20)
	return p
}

// TestHandleSubAgentUpdate_TurnCompletedShowsCompactFinishedCardWithoutPreview
// verifies the chat now surfaces a tiny "turn finished" card when a subagent
// completes a turn, without leaking the response preview into the transcript.
// The preview suppression remains the rule: any concrete content stays inside
// an explicit subagent_inspect call.
func TestHandleSubAgentUpdate_TurnCompletedShowsCompactFinishedCardWithoutPreview(t *testing.T) {
	t.Parallel()
	p := newTurnCompletedTestPage(t)

	const preview = "this response preview must never appear in the chat"
	_, cmd := p.handleRuntimeEvent(&runtime.SubAgentUpdateEvent{
		SessionID: "root-1",
		Envelope: subagent.Envelope{
			SubAgentID:      "child-abcdef1234",
			ParentSessionID: "root-1",
			AgentName:       "worker",
			Kind:            subagent.UpdateKindTurnCompleted,
			Status:          subagent.StatusWaiting,
			Preview:         preview,
		},
	})
	require.NotNil(t, cmd, "forward-to-sidebar path should still emit a command")

	view := p.messages.View()
	assert.NotContains(t, view, preview,
		"turn-completed envelopes must not render the preview in the transcript")
	assert.Contains(t, view, "turn finished",
		"turn-completed envelopes should render a compact 'turn finished' card")
	assert.Contains(t, view, "worker",
		"turn-completed cards should show the child agent badge")
	assert.Contains(t, view, "(child)",
		"turn-completed cards should show the short id in parentheses")
}

// TestHandleSubAgentUpdate_TerminalKindsStillRenderCards makes sure the
// targeted suppression does not accidentally drop the useful terminal
// lifecycle cards (closed / stopped / failed). Failed cards specifically
// carry the error detail, which users need.
func TestHandleSubAgentUpdate_TerminalKindsStillRenderCards(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		envelope   subagent.Envelope
		wantInView string
	}{
		{
			name: "closed renders a compact finalize row",
			envelope: subagent.Envelope{
				SubAgentID: "child-abcdef1234",
				AgentName:  "worker",
				Kind:       subagent.UpdateKindClosed,
				Status:     subagent.StatusClosed,
			},
			wantInView: "◇",
		},
		{
			name: "stopped renders a compact stop row",
			envelope: subagent.Envelope{
				SubAgentID: "child-abcdef1234",
				AgentName:  "worker",
				Kind:       subagent.UpdateKindStopped,
				Status:     subagent.StatusStopped,
			},
			wantInView: "■",
		},
		{
			name: "failed renders its error detail",
			envelope: subagent.Envelope{
				SubAgentID: "child-abcdef1234",
				AgentName:  "worker",
				Kind:       subagent.UpdateKindFailed,
				Status:     subagent.StatusFailed,
				Error:      "oops boom",
			},
			wantInView: "oops boom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTurnCompletedTestPage(t)

			_, cmd := p.handleRuntimeEvent(&runtime.SubAgentUpdateEvent{
				SessionID: "root-1",
				Envelope:  tc.envelope,
			})
			require.NotNil(t, cmd)

			view := p.messages.View()
			assert.Contains(t, view, tc.wantInView,
				"expected view to contain %q for %s", tc.wantInView, tc.name)
		})
	}
}

// TestResolveSubAgentShortRef_Pure exercises the pure short-ref→full-id
// resolution logic used by handleOpenSubAgentByShortRef. The full open-tab
// integration lives in the top-level TUI handler; isolating the resolver here
// keeps coverage sharp without standing up a fake runtime in this package.
func TestLiveTreeRootID_PrefersRecordedRootForNestedSubsessions(t *testing.T) {
	t.Parallel()

	ss := service.NewSessionState(session.New(session.WithParentID("parent-1")))
	ss.SetSubSession(true)
	ss.SetParentSessionID("parent-1")
	ss.SetRootSessionID("root-1")

	p := &chatPage{sessionState: ss}
	assert.Equal(t, "root-1", p.liveTreeRootID(),
		"nested attached tabs should resolve subagent refs against the root tree, not just their immediate parent")
}

func TestLiveTreeRootID_FallsBackToSessionIDForOwnedTabs(t *testing.T) {
	t.Parallel()

	sess := session.New()
	ss := service.NewSessionState(sess)
	p := &chatPage{sessionState: ss}

	// Owned tabs seed RootSessionID from the session state itself, so the helper
	// resolves without needing an attached parent/root chain.
	assert.Equal(t, sess.ID, p.liveTreeRootID())
}

func TestResolveSubAgentShortRef_Pure(t *testing.T) {
	t.Parallel()

	tree := []runtime.LiveSessionNode{
		{SessionID: "root-1", RootSessionID: "root-1", Kind: runtime.LiveSessionRoot},
		{SessionID: "abcde12345xyz", RootSessionID: "root-1", Kind: runtime.LiveSessionSubAgent},
		{SessionID: "vwxyz67890xyz", RootSessionID: "root-1", Kind: runtime.LiveSessionSubAgent},
	}

	assert.Equal(t, "abcde12345xyz", resolveSubAgentShortRef(tree, "abcde"))
	assert.Equal(t, "vwxyz67890xyz", resolveSubAgentShortRef(tree, "vwxyz"))
	assert.Empty(t, resolveSubAgentShortRef(tree, "missing"),
		"unmatched refs must return an empty full id so callers can surface a friendly notice")
	assert.Empty(t, resolveSubAgentShortRef(tree, ""),
		"an empty ref must never match the root id or any subagent row")
	assert.Empty(t, resolveSubAgentShortRef(nil, "abcde"),
		"a nil tree (runtime without SessionTreeProvider) must be handled gracefully")
}
