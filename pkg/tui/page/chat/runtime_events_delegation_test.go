package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newTestChatPageForEvents creates a minimal chatPage for testing event handling.
func newTestChatPageForEvents() *chatPage {
	sessionState := &service.SessionState{}
	return &chatPage{
		messages:     messages.New(sessionState),
		sessionState: sessionState,
	}
}

func TestDelegateToolCallsSuppressed(t *testing.T) {
	t.Parallel()

	// Test PartialToolCallEvent with delegate tool
	t.Run("PartialToolCall", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: builtin.ToolNameDelegate, Arguments: `{}`},
			},
			ToolDefinition: &tools.Tool{Name: builtin.ToolNameDelegate},
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})

	// Test ToolCallEvent with delegate tool
	t.Run("ToolCall", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.ToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: builtin.ToolNameDelegate, Arguments: `{}`},
			},
			ToolDefinition: tools.Tool{Name: builtin.ToolNameDelegate},
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})

	// Test ToolCallResponseEvent with delegate tool
	t.Run("ToolCallResponse", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.ToolCallResponseEvent{
			ToolCallID:     "call-1",
			ToolDefinition: tools.Tool{Name: builtin.ToolNameDelegate},
			Response:       "result",
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})
}

func TestContinueDelegationToolCallsSuppressed(t *testing.T) {
	t.Parallel()

	// Test PartialToolCallEvent with continue_delegation tool
	t.Run("PartialToolCall", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: builtin.ToolNameContinueDelegation, Arguments: `{}`},
			},
			ToolDefinition: &tools.Tool{Name: builtin.ToolNameContinueDelegation},
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})

	// Test ToolCallEvent with continue_delegation tool
	t.Run("ToolCall", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.ToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: builtin.ToolNameContinueDelegation, Arguments: `{}`},
			},
			ToolDefinition: tools.Tool{Name: builtin.ToolNameContinueDelegation},
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})

	// Test ToolCallResponseEvent with continue_delegation tool
	t.Run("ToolCallResponse", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.ToolCallResponseEvent{
			ToolCallID:     "call-1",
			ToolDefinition: tools.Tool{Name: builtin.ToolNameContinueDelegation},
			Response:       "result",
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})
}

func TestStopDelegationToolCallsSuppressed(t *testing.T) {
	t.Parallel()

	// Test PartialToolCallEvent with stop_delegation tool
	t.Run("PartialToolCall", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: builtin.ToolNameStopDelegation, Arguments: `{}`},
			},
			ToolDefinition: &tools.Tool{Name: builtin.ToolNameStopDelegation},
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})

	// Test ToolCallEvent with stop_delegation tool
	t.Run("ToolCall", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.ToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: builtin.ToolNameStopDelegation, Arguments: `{}`},
			},
			ToolDefinition: tools.Tool{Name: builtin.ToolNameStopDelegation},
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})

	// Test ToolCallResponseEvent with stop_delegation tool
	t.Run("ToolCallResponse", func(t *testing.T) {
		t.Parallel()
		p := newTestChatPageForEvents()
		event := &runtime.ToolCallResponseEvent{
			ToolCallID:     "call-1",
			ToolDefinition: tools.Tool{Name: builtin.ToolNameStopDelegation},
			Response:       "result",
		}

		handled, cmd := p.handleRuntimeEvent(event)
		assert.True(t, handled, "expected event to be handled")
		assert.Nil(t, cmd, "expected no command for suppressed tool")
	})
}
