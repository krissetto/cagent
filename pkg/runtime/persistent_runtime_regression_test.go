package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestRunSubSessionCollecting_CanceledBackgroundTaskReturnsStoppedAndDoesNotPersist(t *testing.T) {
	store := session.NewInMemorySessionStore()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("partial").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("start"))
	parent.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), parent))
	_, err = store.AddMessage(t.Context(), parent.ID, session.UserMessage("start"))
	require.NoError(t, err)

	child := session.New(session.WithParentID(parent.ID), session.WithAgentName("worker"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := rt.runSubSessionCollecting(ctx, parent, child, nil)
	require.True(t, result.Stopped)
	require.Empty(t, result.ErrMsg)
	require.Empty(t, result.Result)
	assert.Len(t, parent.Messages, 1, "canceled child should not be attached in memory")

	persisted, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)
	assert.Len(t, persisted.Messages, 1, "canceled child should not be persisted")
}

func TestRunSubSessionForwarding_CanceledTransferReturnsErrorAndDoesNotPersist(t *testing.T) {
	store := session.NewInMemorySessionStore()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("partial").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("start"))
	parent.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), parent))
	_, err = store.AddMessage(t.Context(), parent.ID, session.UserMessage("start"))
	require.NoError(t, err)

	child := session.New(session.WithParentID(parent.ID), session.WithAgentName("worker"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	evts := make(chan Event, 32)

	result, err := rt.runSubSessionForwarding(ctx, parent, child, trace.SpanFromContext(ctx), evts, "root")
	require.ErrorIs(t, err, errSubSessionCanceled)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "Task transfer was canceled")
	assert.Len(t, parent.Messages, 1, "canceled child should not be attached in memory")

	var emittedSubSessionCompleted bool
	close(evts)
	for evt := range evts {
		_, isCompleted := evt.(*SubSessionCompletedEvent)
		emittedSubSessionCompleted = emittedSubSessionCompleted || isCompleted
	}
	assert.False(t, emittedSubSessionCompleted, "canceled transfer should not emit sub-session completion")

	persisted, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)
	assert.Len(t, persisted.Messages, 1, "canceled child should not be persisted")
}

func TestRunSubSessionForwarding_SuccessEmitsSingleSubSessionCompletedEventAndPersistsChild(t *testing.T) {
	store := session.NewInMemorySessionStore()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("delegated result").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("start"))
	parent.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), parent))
	_, err = store.AddMessage(t.Context(), parent.ID, session.UserMessage("start"))
	require.NoError(t, err)

	child := session.New(session.WithParentID(parent.ID), session.WithAgentName("worker"))
	evts := make(chan Event, 128)

	result, err := rt.runSubSessionForwarding(t.Context(), parent, child, trace.SpanFromContext(t.Context()), evts, "root")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Equal(t, "delegated result", result.Output)

	assert.Len(t, parent.Messages, 2)
	require.NotNil(t, parent.Messages[1].SubSession)
	assert.Equal(t, child.ID, parent.Messages[1].SubSession.ID)
	assert.Same(t, parent, parent.Messages[1].SubSession.ParentSession())

	var completedEvents []*SubSessionCompletedEvent
	close(evts)
	for evt := range evts {
		completed, ok := evt.(*SubSessionCompletedEvent)
		if !ok {
			continue
		}
		completedEvents = append(completedEvents, completed)
	}
	require.Len(t, completedEvents, 1)
	assert.Equal(t, parent.ID, completedEvents[0].ParentSessionID)
	require.IsType(t, &session.Session{}, completedEvents[0].SubSession)
	persistedChild, ok := completedEvents[0].SubSession.(*session.Session)
	require.True(t, ok)
	assert.Equal(t, child.ID, persistedChild.ID)

	pr := &PersistentRuntime{LocalRuntime: rt}
	pr.handleEvent(t.Context(), parent, completedEvents[0], &streamingState{})

	persisted, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)
	var persistedSubSessions []*session.Session
	for _, item := range persisted.Messages {
		if item.SubSession != nil {
			persistedSubSessions = append(persistedSubSessions, item.SubSession)
		}
	}
	require.Len(t, persistedSubSessions, 1)
	assert.Equal(t, child.ID, persistedSubSessions[0].ID)
	assert.Equal(t, "delegated result", persistedSubSessions[0].Messages[0].Message.Message.Content)
}

func TestEnsureSubSessionParentMaterialized_RecursivelyPersistsDeepActiveChain(t *testing.T) {
	dbPath := t.TempDir() + "/session.db"
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("deep result").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	rootSession := session.New(session.WithUserMessage("start"))
	rootSession.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), rootSession))
	_, err = store.AddMessage(t.Context(), rootSession.ID, session.UserMessage("start"))
	require.NoError(t, err)

	level1 := session.New(session.WithParentID(rootSession.ID), session.WithAgentName("worker"))
	rootSession.AddSubSession(level1)
	level2 := session.New(session.WithParentID(level1.ID), session.WithAgentName("worker"))
	level1.AddSubSession(level2)
	child := session.New(session.WithParentID(level2.ID), session.WithAgentName("worker"))

	result := rt.runSubSessionCollecting(t.Context(), level2, child, nil)
	require.Empty(t, result.ErrMsg)
	assert.Equal(t, "deep result", result.Result)

	persistedRoot, err := store.GetSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, persistedRoot.Messages, 2)
	persistedLevel1 := persistedRoot.Messages[1].SubSession
	require.NotNil(t, persistedLevel1)
	require.Len(t, persistedLevel1.Messages, 1)
	persistedLevel2 := persistedLevel1.Messages[0].SubSession
	require.NotNil(t, persistedLevel2)
	require.Len(t, persistedLevel2.Messages, 1)
	persistedChild := persistedLevel2.Messages[0].SubSession
	require.NotNil(t, persistedChild)
	assert.Equal(t, child.ID, persistedChild.ID)
	assert.Equal(t, "deep result", persistedChild.Messages[0].Message.Message.Content)
}

func TestRunSubSessionCollecting_PersistsNestedBackgroundSubSession_SQLite(t *testing.T) {
	dbPath := t.TempDir() + "/session.db"
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("nested background result").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	rootSession := session.New(session.WithUserMessage("start"))
	rootSession.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), rootSession))
	_, err = store.AddMessage(t.Context(), rootSession.ID, session.UserMessage("start"))
	require.NoError(t, err)

	parentSub := session.New(session.WithParentID(rootSession.ID), session.WithAgentName("worker"))
	require.NoError(t, store.AddSubSession(t.Context(), rootSession.ID, parentSub))

	child := session.New(session.WithParentID(parentSub.ID), session.WithAgentName("worker"))
	result := rt.runSubSessionCollecting(t.Context(), parentSub, child, nil)
	require.Empty(t, result.ErrMsg)
	assert.Equal(t, "nested background result", result.Result)

	persistedRoot, err := store.GetSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, persistedRoot.Messages, 2)
	require.NotNil(t, persistedRoot.Messages[1].SubSession)
	require.Len(t, persistedRoot.Messages[1].SubSession.Messages, 1)
	require.NotNil(t, persistedRoot.Messages[1].SubSession.Messages[0].SubSession)
	persistedChild := persistedRoot.Messages[1].SubSession.Messages[0].SubSession
	assert.Equal(t, child.ID, persistedChild.ID)
	assert.Equal(t, chat.MessageRoleAssistant, persistedChild.Messages[0].Message.Message.Role)
	assert.Equal(t, "nested background result", persistedChild.Messages[0].Message.Message.Content)
}

func TestRunSubSessionCollecting_ActiveNestedParentWithoutStoreRow_PersistsChild(t *testing.T) {
	dbPath := t.TempDir() + "/session.db"
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("nested background result").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	rootSession := session.New(session.WithUserMessage("start"))
	rootSession.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), rootSession))
	_, err = store.AddMessage(t.Context(), rootSession.ID, session.UserMessage("start"))
	require.NoError(t, err)

	activeParentSub := session.New(session.WithParentID(rootSession.ID), session.WithAgentName("worker"))
	rootSession.AddSubSession(activeParentSub)

	child := session.New(session.WithParentID(activeParentSub.ID), session.WithAgentName("worker"))
	result := rt.runSubSessionCollecting(t.Context(), activeParentSub, child, nil)
	require.Empty(t, result.ErrMsg)
	assert.Equal(t, "nested background result", result.Result)

	persistedRoot, err := store.GetSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, persistedRoot.Messages, 2)
	require.NotNil(t, persistedRoot.Messages[1].SubSession)
	require.Equal(t, activeParentSub.ID, persistedRoot.Messages[1].SubSession.ID)
	require.Len(t, persistedRoot.Messages[1].SubSession.Messages, 1)
	require.NotNil(t, persistedRoot.Messages[1].SubSession.Messages[0].SubSession)
	persistedChild := persistedRoot.Messages[1].SubSession.Messages[0].SubSession
	assert.Equal(t, child.ID, persistedChild.ID)
	assert.Equal(t, chat.MessageRoleAssistant, persistedChild.Messages[0].Message.Message.Role)
	assert.Equal(t, "nested background result", persistedChild.Messages[0].Message.Message.Content)
}

func TestRunSubSessionCollecting_ActiveNestedParentWithoutStoreRow_DoesNotDuplicateChildInMemory(t *testing.T) {
	store := session.NewInMemorySessionStore()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("nested background result").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	rootSession := session.New(session.WithUserMessage("start"))
	rootSession.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), rootSession))
	_, err = store.AddMessage(t.Context(), rootSession.ID, session.UserMessage("start"))
	require.NoError(t, err)

	activeParentSub := session.New(session.WithParentID(rootSession.ID), session.WithAgentName("worker"))
	rootSession.AddSubSession(activeParentSub)

	child := session.New(session.WithParentID(activeParentSub.ID), session.WithAgentName("worker"))
	result := rt.runSubSessionCollecting(t.Context(), activeParentSub, child, nil)
	require.Empty(t, result.ErrMsg)

	persistedRoot, err := store.GetSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, persistedRoot.Messages, 2)
	parentSub := persistedRoot.Messages[1].SubSession
	require.NotNil(t, parentSub)
	require.Len(t, parentSub.Messages, 1)
	require.NotNil(t, parentSub.Messages[0].SubSession)
	assert.Equal(t, child.ID, parentSub.Messages[0].SubSession.ID)
}

func TestNestedForwardedParentMaterializationIsIdempotentAcrossFinalCompletion(t *testing.T) {
	dbPath := t.TempDir() + "/session.db"
	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("nested background result").AddStopWithUsage(4, 2).Build()}
	worker := agent.New("worker", "Worker agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, worker))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)

	rootSession := session.New(session.WithUserMessage("start"))
	rootSession.CreatedAt = time.Now()
	require.NoError(t, store.UpdateSession(t.Context(), rootSession))
	_, err = store.AddMessage(t.Context(), rootSession.ID, session.UserMessage("start"))
	require.NoError(t, err)

	activeParentSub := session.New(session.WithParentID(rootSession.ID), session.WithAgentName("worker"))
	rootSession.AddSubSession(activeParentSub)

	child := session.New(session.WithParentID(activeParentSub.ID), session.WithAgentName("worker"))
	result := rt.runSubSessionCollecting(t.Context(), activeParentSub, child, nil)
	require.Empty(t, result.ErrMsg)
	require.NoError(t, store.AddSubSession(t.Context(), rootSession.ID, activeParentSub))
	_, err = store.AddMessage(t.Context(), rootSession.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "parent continues"}))
	require.NoError(t, err)

	reloadedRoot, err := store.GetSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	require.Len(t, reloadedRoot.Messages, 3)
	parentSub := reloadedRoot.Messages[1].SubSession
	require.NotNil(t, parentSub)
	require.Len(t, parentSub.Messages, 1)
	nestedChild := parentSub.Messages[0].SubSession
	require.NotNil(t, nestedChild)
	require.Len(t, nestedChild.Messages, 1)
	assert.Equal(t, child.ID, nestedChild.ID)
	assert.Equal(t, "nested background result", nestedChild.Messages[0].Message.Message.Content)
	assert.Equal(t, "parent continues", reloadedRoot.Messages[2].Message.Message.Content)
}
