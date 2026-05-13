package dialog

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestToolConfirmationDialog_SubagentStartShowsDelegationHeaderAndFullTask
// verifies the special-cased subagent_start presentation:
//   - A header that reads "[parent] wants to delegate to [child]".
//   - The full task contents rendered underneath.
//   - The plain “parent → child” relationship line is suppressed in this branch
//     because the header already conveys it more prominently.
//
// The chat-side compact rendering (pkg/tui/components/tool/subagent) is not
// touched by this change — we only want the confirmation dialog to be chatty.
func TestToolConfirmationDialog_SubagentStartShowsDelegationHeaderAndFullTask(t *testing.T) {
	t.Parallel()

	ss := service.NewSessionState(session.New())
	ss.SetCurrentAgentName("root")

	const task = "Research recent CI failures and summarise the common root causes."

	dlg := NewToolConfirmationDialog(&runtime.ToolCallConfirmationEvent{
		AgentContext: runtime.AgentContext{AgentName: "root"},
		ToolCall: tools.ToolCall{
			ID:   "tc1",
			Type: "function",
			Function: tools.FunctionCall{
				Name:      subagent.ToolNameStart,
				Arguments: `{"agent":"planner","task":"` + task + `"}`,
			},
		},
	}, ss)
	require.NotNil(t, dlg)
	_ = dlg.SetSize(120, 40)

	view := dlg.View()
	assert.Contains(t, view, "root", "delegation header should name the calling parent agent")
	assert.Contains(t, view, "planner", "delegation header should name the target child agent")
	assert.Contains(t, view, "wants to delegate to",
		"subagent_start confirmations should use the delegation phrasing")
	assert.Contains(t, view, task, "the full task contents should be shown below the delegation header")
	assert.NotContains(t, view, "Expected output:",
		"runtime-managed subagent delegation no longer exposes a separate expected output field")

	// The plain `parent → child` relationship line is the legacy presentation;
	// with the new delegation header it would just duplicate the info.
	assert.Equal(t, 0, strings.Count(view, " → "),
		"the legacy `parent → child` relationship line must not render alongside the delegation header")
}

// TestToolConfirmationDialog_SubagentStartFallsBackToSessionAgent verifies that
// when the tool call event doesn't carry an AgentName (some older replay paths
// lose it), we fall back to the session's current agent name rather than
// rendering an empty parent badge.
func TestToolConfirmationDialog_SubagentStartFallsBackToSessionAgent(t *testing.T) {
	t.Parallel()

	ss := service.NewSessionState(session.New())
	ss.SetCurrentAgentName("orchestrator")

	dlg := NewToolConfirmationDialog(&runtime.ToolCallConfirmationEvent{
		ToolCall: tools.ToolCall{
			ID:   "tc1",
			Type: "function",
			Function: tools.FunctionCall{
				Name:      subagent.ToolNameStart,
				Arguments: `{"agent":"planner","task":"do work"}`,
			},
		},
	}, ss)
	_ = dlg.SetSize(120, 40)

	view := dlg.View()
	assert.Contains(t, view, "orchestrator",
		"parent should fall back to the session's current agent when the event omits AgentName")
	assert.Contains(t, view, "planner")
	assert.Contains(t, view, "wants to delegate to")
}

// TestToolConfirmationDialog_SubagentRelationshipLineUsesParentAndChildNames
// locks in the plain `parent → child` relationship line for non-delegation
// subagent tools (send/finalize/stop/inspect). subagent_start has its own
// delegation header and is covered by the dedicated test above.
func TestToolConfirmationDialog_SubagentRelationshipLineUsesParentAndChildNames(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("parent-1"))
	ss := service.NewSessionState(sess)
	ss.SetSubSession(true)
	ss.SetCurrentAgentName("worker")
	ss.SetParentAgentName("planner")

	dlg := NewToolConfirmationDialog(&runtime.ToolCallConfirmationEvent{
		ToolCall: tools.ToolCall{
			ID:   "tc1",
			Type: "function",
			Function: tools.FunctionCall{
				Name:      subagent.ToolNameSend,
				Arguments: `{"subagent_id":"abcde","message":"ping"}`,
			},
		},
	}, ss)
	require.NotNil(t, dlg)
	_ = dlg.SetSize(100, 40)

	view := dlg.View()
	assert.Contains(t, view, "planner")
	assert.Contains(t, view, "worker")
	assert.Contains(t, view, "→",
		"non-delegation subagent tool confirmations should still show the parent-to-child relationship line")
}
