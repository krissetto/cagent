package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSubagentStartInheritsSessionFields(t *testing.T) {
	root := session.New(
		session.WithID("root"),
		session.WithWorkingDir("/work"),
		session.WithToolsApproved(true),
		session.WithExcludedTools([]string{"shell"}),
		session.WithAttachedFiles([]string{"/work/a.txt"}),
	)
	child := session.NewRuntimeManagedSubSession(root,
		session.WithAgentName("child"),
		session.WithWorkingDir(root.WorkingDir),
		session.WithToolsApproved(root.ToolsApproved),
		session.WithExcludedTools(root.ExcludedTools),
		session.WithAttachedFiles(root.AttachedFiles),
	)
	assert.Equal(t, root.WorkingDir, child.WorkingDir)
	assert.Equal(t, root.ToolsApproved, child.ToolsApproved)
	assert.Equal(t, root.ExcludedTools, child.ExcludedTools)
	assert.Equal(t, root.AttachedFiles, child.AttachedFiles)
}

func TestSubagentSendRejectsWhitespace(t *testing.T) {
	r := &LocalRuntime{now: time.Now}
	m := NewSubagentManager(r)
	root := session.New(session.WithID("root"))
	h := &subagentHandle{id: "abcde-1", shortID: "abcde", parent: root, sess: session.NewRuntimeManagedSubSession(root, session.WithID("abcde-1")), done: make(chan struct{}), inbox: make(chan string, 1)}
	m.all[h.id] = h
	err := m.Send(root, "abcde", "   ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestSubagentCrossRootRejected(t *testing.T) {
	r := &LocalRuntime{now: time.Now}
	m := NewSubagentManager(r)
	root1 := session.New(session.WithID("root1"))
	root2 := session.New(session.WithID("root2"))
	h := &subagentHandle{id: "child123", shortID: "child", parent: root1, sess: session.NewRuntimeManagedSubSession(root1, session.WithID("child123")), done: make(chan struct{})}
	m.all[h.id] = h
	_, err := m.resolve(root2, "child")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-root")
}

func TestSubagentEnvelopePersistedBeforeHandoffAndNotPublicQueue(t *testing.T) {
	root := session.New(session.WithID("root"))
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.UpdateSession(t.Context(), root))
	r := &LocalRuntime{sessionStore: store, steerQueue: rejectingQueue{}, followUpQueue: rejectingQueue{}}
	m := NewSubagentManager(r)
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child"))
	h := &subagentHandle{id: child.ID, shortID: "child", parent: root, sess: child, envelopes: []string{"done"}, done: make(chan struct{})}
	m.all[h.id] = h
	var events []Event
	drained := m.DrainEnvelopes(t.Context(), root, EventSinkFunc(func(ev Event) { events = append(events, ev) }))
	require.True(t, drained)
	got, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, session.MessageKindSubagentEnvelope, got.Messages[0].Message.Kind)
	require.Len(t, events, 1)
	assert.IsType(t, &UserMessageEvent{}, events[0])
}

func TestParentIdleResumeEventsStableType(t *testing.T) {
	idle, err := json.Marshal(ParentIdle("root", 2, []string{"a", "b"}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"parent_idle","session_id":"root","count":2,"ids":["a","b"]}`, string(idle))
	resume, err := json.Marshal(ParentResume("root", 1, []string{"a"}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"parent_resume","session_id":"root","count":1,"ids":["a"]}`, string(resume))
}

type rejectingQueue struct{}

func (rejectingQueue) Enqueue(context.Context, QueuedMessage) bool   { panic("public queue used") }
func (rejectingQueue) Dequeue(context.Context) (QueuedMessage, bool) { return QueuedMessage{}, false }
func (rejectingQueue) Drain(context.Context) []QueuedMessage         { return nil }
