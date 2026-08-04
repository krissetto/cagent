package messages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// The animation coordinator is package-global: a leaked subscription keeps
// the tick stream alive for the rest of the process. Not parallel — the test
// asserts on the global coordinator.
func TestStopAnimationsReleasesEverySubscription(t *testing.T) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 80, 24, &service.SessionState{})

	// A completed tool call replaces the running view; the old view's
	// spinner subscription must not survive the swap.
	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "list"}}
	_ = m.AddOrUpdateToolCall("agent", call, tools.Tool{Name: "list"}, types.ToolStatusRunning)
	_ = m.View() // running view renders and registers its spinner
	require.True(t, ar.HasActive())
	_ = m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "call-1", Response: "done"}, types.ToolStatusCompleted)

	// A waiting spinner still animating when the list is discarded.
	_ = m.AddAssistantMessage("agent", "")
	_ = m.View()

	m.StopAnimations()
	require.False(t, ar.HasActive(),
		"discarding the list must release every animation subscription")
}
