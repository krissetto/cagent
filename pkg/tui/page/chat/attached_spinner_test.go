package chat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newAttachedTestChatPage builds a chat page wired to an attached-subagent
// sessionState and a real messages model. It avoids bringing in a full *app.App
// by leaving a.app nil and only exercising code paths that do not touch it.
func newAttachedTestChatPage(t *testing.T) *chatPage {
	t.Helper()

	sess := session.New(session.WithParentID("parent-1"))
	ss := service.NewSessionState(sess)
	require.True(t, ss.IsSubSession(), "test fixture must represent a sub-session")

	return &chatPage{
		sidebar:      sidebar.New(ss),
		messages:     messages.New(ss),
		sessionState: ss,
	}
}

// TestSetWorking_PublicSetter verifies the public SetWorking on chat.Page
// actually flips the working state. This is the path handleOpenSubAgentTab
// uses to seed the spinner when attaching mid-turn.
func TestSetWorking_PublicSetter(t *testing.T) {
	t.Parallel()
	p := newAttachedTestChatPage(t)

	assert.False(t, p.IsWorking(), "page starts not working")

	cmd := p.SetWorking(true)
	assert.NotNil(t, cmd, "state transition should emit a WorkingStateChangedMsg cmd")
	assert.True(t, p.IsWorking(), "SetWorking(true) must flip the working state")

	// Second call with the same value is a no-op — no cmd, state unchanged.
	cmd = p.SetWorking(true)
	assert.Nil(t, cmd, "idempotent true→true must not emit a cmd")
	assert.True(t, p.IsWorking())

	cmd = p.SetWorking(false)
	assert.NotNil(t, cmd, "true→false must emit a cmd")
	assert.False(t, p.IsWorking())
}

// TestUserMessageEvent_FlipsWorkingInAttachedTab verifies that the
// UserMessageEvent path turns on the working indicator for attached subagent
// tabs. Before this fix, the spinner only appeared once the child emitted its
// StreamStartedEvent — which can be delayed by slow tool loading — so users
// saw no feedback between hitting send and first content arriving.
func TestUserMessageEvent_FlipsWorkingInAttachedTab(t *testing.T) {
	t.Parallel()
	p := newAttachedTestChatPage(t)

	// Child loop publishes UserMessage on the child's bus when it drains
	// the inbox. In attached mode this is the earliest reliable "new turn
	// starting" signal the UI gets, so it must turn working on.
	handled, cmd := p.handleRuntimeEvent(runtime.UserMessage("hello child", p.sessionState.SessionTitle(), nil, 0))
	require.True(t, handled, "UserMessageEvent should be handled by the chat page")
	require.NotNil(t, cmd, "handler must return a batched cmd")
	assert.True(t, p.IsWorking(), "attached tab must show working=true on UserMessageEvent")
}

// TestUserMessageEvent_SuppressesSubagentEnvelopeReminder verifies the earlier
// behavior is preserved: subagent_update envelope reminders must not render
// (and, crucially, must not flip working — the parent's own turn lifecycle
// owns that state, not an injected reminder).
func TestUserMessageEvent_SuppressesSubagentEnvelopeReminder(t *testing.T) {
	t.Parallel()
	p := newAttachedTestChatPage(t)

	handled, cmd := p.handleRuntimeEvent(runtime.TypedUserMessage(session.MessageKindSubagentEnvelope, "<subagent_update>x</subagent_update>", "", nil, 0))
	require.True(t, handled)
	assert.Nil(t, cmd, "envelope-reminder path should be a no-op")
	assert.False(t, p.IsWorking(), "envelope reminders must not touch working state")
}

// TestUserMessageEvent_DoesNotFlipWorkingForOwnedTab ensures the new
// "attached tab turns working on at UserMessage" behavior is scoped to
// attached sub-session tabs only. Owned tabs already drive working state
// synchronously from processMessage → setWorking before the event round-trip,
// so we don't want UserMessageEvent to also flip it — it would mask real
// cases where setWorking had been turned off (e.g. after tool confirmation
// cleared it).
func TestUserMessageEvent_DoesNotFlipWorkingForOwnedTab(t *testing.T) {
	t.Parallel()

	sess := session.New() // no ParentID → owned (root) session
	ss := service.NewSessionState(sess)
	require.False(t, ss.IsSubSession())

	p := &chatPage{
		sidebar:      sidebar.New(ss),
		messages:     messages.New(ss),
		sessionState: ss,
	}

	handled, _ := p.handleRuntimeEvent(runtime.UserMessage("hi", "", nil, 0))
	require.True(t, handled)
	assert.False(t, p.IsWorking(), "owned tabs must not have their working state altered by UserMessageEvent")
}

// TestRenderSubsessionStrip_ContentDependsOnSessionType verifies the
// transcript strip only shows up in attached sub-session tabs. Owned tabs
// must render an empty string so they don't pay layout/vertical space cost.
func TestRenderSubsessionStrip_ContentDependsOnSessionType(t *testing.T) {
	t.Parallel()

	t.Run("owned tab returns empty", func(t *testing.T) {
		t.Parallel()
		ss := service.NewSessionState(session.New())
		p := &chatPage{sessionState: ss}
		assert.Empty(t, p.renderSubsessionStrip(80))
	})

	t.Run("sub-session tab renders the banner with a one-row margin beneath it", func(t *testing.T) {
		t.Parallel()
		ss := service.NewSessionState(session.New(session.WithParentID("root-1")))
		ss.SetCurrentAgentName("researcher")
		ss.SetParentAgentName("planner")
		p := &chatPage{sessionState: ss}

		strip := p.renderSubsessionStrip(80)
		require.NotEmpty(t, strip)
		assert.Equal(t, 1, strings.Count(strip, "\n"),
			"the transcript strip is the relationship line plus one blank row of bottom margin")
		first, _, _ := strings.Cut(strip, "\n")
		assert.Contains(t, first, "planner")
		assert.Contains(t, first, "researcher")
		assert.Contains(t, first, "↔")
	})

	t.Run("sub-session tab without agent names falls back gracefully", func(t *testing.T) {
		t.Parallel()
		ss := service.NewSessionState(session.New(session.WithParentID("root-1")))
		p := &chatPage{sessionState: ss}

		strip := p.renderSubsessionStrip(80)
		require.NotEmpty(t, strip, "strip must still render during the brief pre-AgentInfo window")
		assert.Contains(t, strip, "parent")
		assert.Contains(t, strip, "child")
		assert.Contains(t, strip, "↔")
	})

	t.Run("zero width returns empty", func(t *testing.T) {
		t.Parallel()
		ss := service.NewSessionState(session.New(session.WithParentID("root-1")))
		ss.SetCurrentAgentName("researcher")
		p := &chatPage{sessionState: ss}
		assert.Empty(t, p.renderSubsessionStrip(0), "renderer must be defensive against zero/negative widths")
	})
}

// TestSetSize_AttachedSubsessionReservesStripHeight exercises the layout path
// that previously caused the subagent relationship banner to push the
// transcript one row too tall. SetSize must reserve the banner's height in the
// messages viewport rather than subtracting it only at render time.
func TestSetSize_AttachedSubsessionReservesStripHeight(t *testing.T) {
	t.Parallel()

	p := newAttachedTestChatPage(t)
	p.sessionState.SetCurrentAgentName("researcher")
	p.sessionState.SetParentAgentName("planner")

	p.SetSize(140, 20)
	assert.Equal(t, 2, p.fixedStripHeight,
		"attached sub-session tabs should reserve one banner row plus one blank spacer row beneath it")

	// Owned tabs should reserve no fixed strip height.
	ownedSS := service.NewSessionState(session.New())
	owned := &chatPage{
		sidebar:      sidebar.New(ownedSS),
		messages:     messages.New(ownedSS),
		sessionState: ownedSS,
	}
	owned.SetSize(140, 20)
	assert.Equal(t, 0, owned.fixedStripHeight,
		"owned tabs must not pay any layout cost for the sub-session strip")
}

// TestView_AttachedSubsessionWithScrollableContentKeepsRequestedHeight is a
// regression test for the original overflow bug: once the transcript becomes
// scrollable, the presence of the fixed banner must not increase the rendered
// page height beyond the height assigned by the parent layout.
func TestView_AttachedSubsessionWithScrollableContentKeepsRequestedHeight(t *testing.T) {
	t.Parallel()

	p := newAttachedTestChatPage(t)
	p.sessionState.SetCurrentAgentName("researcher")
	p.sessionState.SetParentAgentName("planner")

	// Add enough content to guarantee the messages viewport needs scrolling.
	for range 30 {
		_ = p.messages.AddUserMessage(strings.Repeat("line ", 20))
	}

	p.SetSize(140, 12)
	view := p.View()
	assert.Equal(t, 12, strings.Count(view, "\n")+1,
		"the rendered chat page must stay within the assigned height even when the transcript scrolls")
}
