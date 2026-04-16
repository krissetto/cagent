package chat

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newTestChatPageForEvents creates a minimal chatPage for testing event handling.
func newTestChatPageForEvents() *chatPage {
	sessionState := &service.SessionState{}
	return &chatPage{
		messages:     messages.New(sessionState),
		sidebar:      newTestSidebar(),
		sessionState: sessionState,
	}
}

// newTestSidebar creates a mock sidebar for testing.
func newTestSidebar() sidebar.Model {
	return sidebar.New(&service.SessionState{})
}

// newTestChatPageForEventsDirect creates a chatPage with direct access to the underlying messages model for testing.
func newTestChatPageForEventsDirect() (*chatPage, *messages.TestModel) {
	sessionState := &service.SessionState{}
	messagesModel := messages.NewTestModel(sessionState)
	return &chatPage{
		messages:     messagesModel,
		sidebar:      newTestSidebar(),
		sessionState: sessionState,
	}, messagesModel
}

func TestDelegationStartedEvent_SessionIDStoredOnCard(t *testing.T) {
	t.Parallel()

	p, messagesModel := newTestChatPageForEventsDirect()
	event := &runtime.DelegationStartedEvent{
		DelegationID: "ab3k9",
		AgentName:    "bot",
		Task:         "work",
		SessionID:    "child-xyz",
	}

	handled, cmd := p.handleRuntimeEvent(event)
	assert.True(t, handled)

	// DelegationStartedEvent now adds a compact delegation pill to the
	// message transcript and updates the sidebar.
	assert.NotNil(t, cmd)
	assert.NotEmpty(t, messagesModel.GetMessages())

	// Verify the card carries the child session ID for click-to-open.
	card := messagesModel.GetMessages()[0]
	assert.Equal(t, "child-xyz", card.DelegationSessionID)
}

func TestUserMessageEvent_DelegationNotificationUsesAgentName(t *testing.T) {
	t.Parallel()

	p, messagesModel := newTestChatPageForEventsDirect()
	event := &runtime.UserMessageEvent{
		Message:      "worker (ab123) has responded",
		Kind:         "delegation-notification",
		AgentContext: runtime.AgentContext{AgentName: "worker"},
	}

	handled, cmd := p.handleRuntimeEvent(event)
	assert.True(t, handled)
	assert.NotNil(t, cmd)

	msgs := messagesModel.GetMessages()
	require.Len(t, msgs, 1)
	// Delegation notifications are persisted via session.SubagentResultMessage
	// and their AgentName is preserved in the event metadata. The TUI must use
	// AgentName for the compact label; msg.Message already contains the full
	// notification text (e.g. "worker (ab123) has responded").
	assert.Equal(t, "worker responded", msgs[0].Content)
	assert.Equal(t, "worker", msgs[0].Sender)
}

type fakeRuntime struct{}

func (f *fakeRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo { return runtime.CurrentAgentInfo{} }
func (f *fakeRuntime) CurrentAgentName() string                                 { return "root" }
func (f *fakeRuntime) SetCurrentAgent(string) error                              { return nil }
func (f *fakeRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error)   { return nil, nil }
func (f *fakeRuntime) EmitStartupInfo(context.Context, *session.Session, chan runtime.Event) {
}
func (f *fakeRuntime) ResetStartupInfo()                                              {}
func (f *fakeRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event { ch := make(chan runtime.Event); close(ch); return ch }
func (f *fakeRuntime) Run(context.Context, *session.Session) ([]session.Message, error) { return nil, nil }
func (f *fakeRuntime) Resume(context.Context, runtime.ResumeRequest)                    {}
func (f *fakeRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any) error {
	return nil
}
func (f *fakeRuntime) SessionStore() session.Store                              { return nil }
func (f *fakeRuntime) Summarize(context.Context, *session.Session, string, chan runtime.Event) {
}
func (f *fakeRuntime) PermissionsInfo() *runtime.PermissionsInfo                  { return nil }
func (f *fakeRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset          { return nil }
func (f *fakeRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return nil
}
func (f *fakeRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}
func (f *fakeRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error { return nil }
func (f *fakeRuntime) TitleGenerator() *sessiontitle.Generator                             { return nil }
func (f *fakeRuntime) Steer(runtime.QueuedMessage) error                                   { return nil }
func (f *fakeRuntime) FollowUp(runtime.QueuedMessage) error                                { return nil }
func (f *fakeRuntime) Close() error                                                        { return nil }

func TestHandleDelegationResume_IdleParentDoesNotAddImmediateNotification(t *testing.T) {
	t.Parallel()

	p, messagesModel := newTestChatPageForEventsDirect()
	p.app = app.New(context.Background(), &fakeRuntime{}, session.New())
	p.msgCancel = nil
	p.working = false

	msg := msgtypes.DelegationResumeMsg{
		DelegationID: "ab123",
		AgentName:    "worker",
		Content:      "worker (ab123) has responded",
	}

	_, cmd := p.handleDelegationResume(msg)
	assert.NotNil(t, cmd)
	assert.Empty(t, messagesModel.GetMessages(), "idle resume should not add immediate compact notification; runtime queue will add the single transcript entry")

	// Give the async goroutine a moment to run and confirm it still didn't inject
	// a local compact notification directly.
	time.Sleep(10 * time.Millisecond)
	assert.Empty(t, messagesModel.GetMessages())
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
