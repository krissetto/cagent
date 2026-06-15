package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSubagentStartedEventUpdatesSidebarOnlyNotTranscript(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess), WithLeanMode()).(*chatPage)
	p.SetSize(100, 24)

	handled, _ := p.handleRuntimeEvent(&runtime.SubAgentStartedEvent{
		SessionID: "root",
		SubAgent:  runtime.SubagentInfo{ID: "child-123", ShortID: "child", AgentName: "implementer", State: "running"},
	})
	require.True(t, handled)

	plain := strings.TrimSpace(ansi.Strip(p.View()))
	assert.NotContains(t, plain, "started")
	assert.NotContains(t, plain, "implementer")
}

func TestSubagentTurnEnvelopeStillRendersTranscriptRow(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess), WithLeanMode()).(*chatPage)
	p.SetSize(100, 24)

	handled, _ := p.handleRuntimeEvent(runtime.TypedUserMessage(session.MessageKindSubagentEnvelope, "[implementer] (child) turn finished. Preview: done", "root", nil, 0))
	require.True(t, handled)

	plain := ansi.Strip(p.View())
	assert.Contains(t, plain, "implementer")
	assert.Contains(t, plain, "turn finished")
}

func TestTeamInfoInvalidatesRestoredSubagentColorCaches(t *testing.T) {
	styles.SetAgentOrder(nil)
	defer styles.SetAgentOrder(nil)

	sess := session.New(session.WithID("root"))
	sess.AddMessage(&session.Message{AgentName: "root", Message: chatmsg.Message{
		Role: chatmsg.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{{ID: "call-1", Function: tools.FunctionCall{
			Name:      runtime.ToolNameSubagentStart,
			Arguments: `{"agent":"director","task":"ask"}`,
		}}},
		ToolDefinitions: []tools.Tool{{Name: runtime.ToolNameSubagentStart}},
	}})
	sess.AddMessage(&session.Message{Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: "call-1", Content: `{"subagent_id":"338dd"}`}})
	sess.AddMessage(session.SubagentEnvelopeMessage("[director] (338dd) turn finished. Preview: done"))
	state := service.NewSessionState(sess)
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), state, WithLeanMode()).(*chatPage)
	p.SetSize(100, 24)
	_ = p.Init()

	before := p.View()
	state.SetAvailableAgents([]runtime.AgentDetails{{Name: "root"}, {Name: "director"}, {Name: "implementer"}})
	defer state.SetAvailableAgents(nil)
	handled, _ := p.handleRuntimeEvent(&runtime.TeamInfoEvent{AvailableAgents: state.AvailableAgents(), CurrentAgent: "root"})
	require.True(t, handled)
	after := p.View()

	require.Contains(t, ansi.Strip(after), "asking director (338dd)")
	require.Contains(t, ansi.Strip(after), "director (338dd) turn finished")
	directorName := styles.AgentAccentStyleFor("director").Render("director")
	require.GreaterOrEqual(t, strings.Count(after, directorName), 2, "TeamInfo must repaint both restored asking and turn-finished rows with the team agent color")
	require.NotEqual(t, before, after, "TeamInfo must invalidate restored transcript rows so agent colors are recomputed")
}

// TestWorkingState_OwnSessionStartStopClears verifies the page's working
// spinner turns on for its own session's stream and clears when that stream
// stops — the normal single-turn lifecycle.
func TestWorkingState_OwnSessionStartStopClears(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	handled, _ := p.handleRuntimeEvent(runtime.StreamStarted("root", "root"))
	require.True(t, handled)
	assert.True(t, p.IsWorking(), "own-session stream start should set working")

	handled, _ = p.handleRuntimeEvent(runtime.StreamStopped("root", "root", "normal"))
	require.True(t, handled)
	assert.False(t, p.IsWorking(), "own-session stream stop should clear working")
	assert.Equal(t, 0, p.streamDepth)
}

// TestWorkingState_UnmatchedChildStreamDoesNotStickParentSpinner is the
// regression for the stuck root spinner: a descendant session's StreamStarted
// that is never balanced by a matching StreamStopped (e.g. dropped, or the
// child runs as its own session) must not drive the parent's working state,
// so the parent never gets stuck "working" between messages.
func TestWorkingState_UnmatchedChildStreamDoesNotStickParentSpinner(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	// Parent runs and finishes a turn.
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("root", "root"))
	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("root", "root", "normal"))
	require.False(t, p.IsWorking())

	// A child session's stream starts (and, say, its stop is never observed by
	// this page). The parent must not be considered working.
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("child", "greppy"))
	assert.False(t, p.IsWorking(), "child stream must not set parent working")
	assert.Equal(t, 0, p.streamDepth, "child stream must not change parent stream depth")

	// Even a stray child stop must not push the parent negative or flip state.
	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("child", "greppy", "normal"))
	assert.False(t, p.IsWorking())
	assert.Equal(t, 0, p.streamDepth)
}

// TestWorkingState_NestedOwnSessionStreamsBalance verifies nested same-session
// streams (start/start/stop/stop) only clear working at the outermost stop.
func TestWorkingState_NestedOwnSessionStreamsBalance(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("root", "root"))
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("root", "root"))
	assert.True(t, p.IsWorking())
	assert.Equal(t, 2, p.streamDepth)

	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("root", "root", "normal"))
	assert.True(t, p.IsWorking(), "inner stop should keep working until the outer stop")
	assert.Equal(t, 1, p.streamDepth)

	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("root", "root", "normal"))
	assert.False(t, p.IsWorking(), "outer stop clears working")
	assert.Equal(t, 0, p.streamDepth)
}

// TestWorkingState_DelegationSequenceClearsSpinner reproduces the parent's
// event sequence around a runtime-managed subagent delegation and asserts the
// working spinner is not left stuck on between messages:
//
//	StreamStarted(parent)            -> working
//	ParentIdle(parent)               -> parked, working cleared, depth reset
//	subagent envelope (turn_completed user message)
//	ParentResume(parent)             -> resumes
//	StreamStopped(parent)            -> turn over, working stays cleared
func TestWorkingState_DelegationSequenceClearsSpinner(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("root", "root"))
	require.True(t, p.IsWorking())

	_, _ = p.handleRuntimeEvent(&runtime.ParentIdleEvent{SessionID: "root", Count: 1, IDs: []string{"child"}})
	assert.False(t, p.IsWorking(), "parent idle should clear working")
	assert.Equal(t, 0, p.streamDepth, "parent idle should reset stream depth")

	// Subagent envelope arrives as a typed user message on the parent.
	_, _ = p.handleRuntimeEvent(runtime.TypedUserMessage(session.MessageKindSubagentEnvelope, "[director] (e546b) turn finished.", "root", nil, 0))

	_, _ = p.handleRuntimeEvent(&runtime.ParentResumeEvent{SessionID: "root", Count: 1, IDs: []string{"child"}})

	// Final stop for the parent's own turn.
	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("root", "root", "normal"))
	assert.False(t, p.IsWorking(), "working must be cleared after the delegation turn ends")
	assert.Equal(t, 0, p.streamDepth)
}

// TestWorkingState_LateToolEventAfterStopDoesNotRelightSpinner is the precise
// regression for the spinner stuck after a subagent delegation: the trailing
// subagent_start tool response can be delivered after the parent turn's
// StreamStopped (event throttling/ordering). A tool event arriving with no
// active stream must not re-light the working spinner.
func TestWorkingState_LateToolEventAfterStopDoesNotRelightSpinner(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithID("root"))
	p := New(app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)

	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("root", "root"))
	_, _ = p.handleRuntimeEvent(runtime.StreamStopped("root", "root", "normal"))
	require.False(t, p.IsWorking())
	require.Equal(t, 0, p.streamDepth)

	// Late tool response (e.g. trailing subagent_start) lands after the stop.
	_, _ = p.handleRuntimeEvent(runtime.ToolCallResponse("call-1", tools.Tool{}, tools.ResultSuccess("ok"), "ok", "root"))
	assert.False(t, p.IsWorking(), "late tool response must not re-light the spinner")

	// A late tool call event likewise must not re-light it.
	_, _ = p.handleRuntimeEvent(runtime.ToolCall(tools.ToolCall{ID: "call-2"}, tools.Tool{}, "root"))
	assert.False(t, p.IsWorking(), "late tool call must not re-light the spinner")
}
