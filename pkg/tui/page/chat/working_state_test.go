package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
)

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
