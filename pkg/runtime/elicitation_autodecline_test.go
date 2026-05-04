package runtime

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestElicitation_AutoDeclineForNonInteractiveSession verifies that when a
// non-interactive session (e.g. an unattached background subagent) triggers
// an MCP elicitation, the runtime auto-declines immediately instead of
// blocking forever on elicitationRequestCh.
//
// The root cause is that child sessionRunners have their own
// elicitationRequestCh but nobody calls ResumeElicitation on a child
// runner, so the blocking receive never completes.
func TestElicitation_AutoDeclineForNonInteractiveSession(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)

	// Build a non-interactive session (like a subagent child session).
	sess := session.New(
		session.WithUserMessage("task"),
		session.WithNonInteractive(true),
	)

	// Get the handler that configureToolsetHandlers would wire for this session.
	runner := newRootSessionRunner(setup.rt)
	handler := runner.elicitationHandlerFor(sess)
	require.NotNil(t, handler)

	// The handler must return immediately with a decline — no blocking.
	result, err := handler(t.Context(), &mcp.ElicitParams{
		Message: "Please confirm the operation",
	})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action,
		"non-interactive sessions must auto-decline elicitation")
}

// TestElicitation_InteractiveSessionUsesNormalHandler verifies that
// interactive (root) sessions still use the normal blocking elicitation
// handler that waits for a human response via ResumeElicitation.
func TestElicitation_InteractiveSessionUsesNormalHandler(t *testing.T) {
	t.Parallel()

	setup := newSubagentTestSetup(t)

	// Build an interactive session (the default for root sessions).
	sess := session.New(session.WithUserMessage("hi"))

	runner := newRootSessionRunner(setup.rt)
	handler := runner.elicitationHandlerFor(sess)
	require.NotNil(t, handler)

	// Set up the elicitation events channel so the handler can send the event.
	events := make(chan Event, 16)
	runner.swapElicitationEventsChannel(events)

	// Pre-load an elicitation response so the handler doesn't block forever.
	go func() {
		setup.rt.elicitationRequestCh <- ElicitationResult{
			Action:  tools.ElicitationActionAccept,
			Content: map[string]any{"answer": "yes"},
		}
	}()

	result, err := handler(t.Context(), &mcp.ElicitParams{
		Message: "Confirm?",
	})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionAccept, result.Action,
		"interactive sessions must use the normal elicitation handler that waits for a response")
}
