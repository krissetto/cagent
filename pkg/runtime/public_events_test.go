package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestPublicEventObserverPersistsPublicEventsAndSkipsMessageAdded(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPublicEventObserver(store)
	require.NotNil(t, obs)

	obs.OnEvent(t.Context(), sess, StreamStarted(sess.ID, "agent"))
	obs.OnEvent(t.Context(), sess, MessageAdded(sess.ID, session.NewAgentMessage("agent", &chat.Message{Role: chat.MessageRoleAssistant, Content: "internal"}), "agent"))

	events, err := store.(session.PublicRuntimeEventStore).ReplayPublicRuntimeEvents(t.Context(), session.PublicRuntimeEventQuery{RootID: "root"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "root", events[0].SessionID)
	assert.Equal(t, "root", events[0].RootID)
	assert.Equal(t, "session", events[0].Scope)
	assert.Equal(t, "stream_started", events[0].Type)
	assert.Contains(t, events[0].PayloadJSON, `"type":"stream_started"`)
	assert.Contains(t, events[0].PayloadJSON, `"session_id":"root"`)
}

func TestPublicEventObserverUsesScopedSessionIDAndRoot(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"))
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))

	obs := newPublicEventObserver(store)
	obs.OnEvent(t.Context(), root, AgentChoice("agent", child.ID, "hello"))

	events, err := ReplayPublicRuntimeEvents(t.Context(), store, session.PublicRuntimeEventQuery{RootID: "root"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "child", events[0].SessionID)
	assert.Equal(t, "root", events[0].RootID)
	assert.Equal(t, "agent_choice", events[0].Type)
	assert.Contains(t, events[0].PayloadJSON, `"content":"hello"`)
}

func TestReplayPublicRuntimeEventsReturnsNilForUnsupportedStore(t *testing.T) {
	events, err := ReplayPublicRuntimeEvents(t.Context(), nil, session.PublicRuntimeEventQuery{RootID: "root"})
	require.NoError(t, err)
	assert.Nil(t, events)
}
