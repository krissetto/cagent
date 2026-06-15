package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
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
