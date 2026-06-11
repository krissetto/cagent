package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestByIDChildRoutingTargetsChildQueuesAndSnapshots(t *testing.T) {
	rt, root, child := testLiveControlRuntime(t, "child-alpha")

	require.NoError(t, rt.FollowUpSessionByID(child.ID, QueuedMessage{Content: "child follow"}))
	require.NoError(t, rt.SteerSessionByID(child.ID, QueuedMessage{Content: "child steer"}))

	rootQueues := rt.queuesFor(root)
	assert.Empty(t, rootQueues.followUp.(QueueSnapshotter).Snapshot())
	assert.Empty(t, rootQueues.steer.(QueueSnapshotter).Snapshot())

	childQueues := rt.queuesFor(child)
	assert.Equal(t, []QueuedMessage{{Content: "child follow"}}, childQueues.followUp.(QueueSnapshotter).Snapshot())
	assert.Equal(t, []QueuedMessage{{Content: "child steer"}}, childQueues.steer.(QueueSnapshotter).Snapshot())

	snapshot, events, err := rt.AttachLiveSessionWithSnapshot(t.Context(), child.ID, 8)
	require.NoError(t, err)
	assert.NotNil(t, events)
	require.NotEmpty(t, snapshot)
	queueEvent, ok := snapshot[len(snapshot)-1].(*SessionQueueEvent)
	require.True(t, ok)
	assert.Equal(t, child.ID, queueEvent.SessionID)
	assert.Equal(t, 1, queueEvent.Count)
	assert.Equal(t, []string{"child follow"}, queueEvent.Previews)
}

func TestByIDRoutingRejectsCrossRootAndAmbiguousShortIDs(t *testing.T) {
	rt, _, child := testLiveControlRuntime(t, "abcde-child-one")
	otherRoot := session.New(session.WithID("other-root"))
	otherChild := session.NewRuntimeManagedSubSession(otherRoot, session.WithID("abcde-child-two"), session.WithAgentName("reviewer"))
	rt.subagents.all[otherChild.ID] = &subagentHandle{
		id:        otherChild.ID,
		shortID:   shortID(otherChild.ID),
		agentName: "reviewer",
		parent:    otherRoot,
		sess:      otherChild,
		created:   time.Now(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		state:     "running",
	}

	err := rt.FollowUpSessionByID(otherChild.ID, QueuedMessage{Content: "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-root")

	err = rt.SteerSessionByID("abcde", QueuedMessage{Content: "ambiguous"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")

	require.NoError(t, rt.SteerSessionByID(child.ID, QueuedMessage{Content: "direct ok"}))
}

func TestLiveChildSessionAndAttachRejectUnknown(t *testing.T) {
	rt, _, child := testLiveControlRuntime(t, "child-alpha")
	got, ok := rt.LiveChildSession(child.ID)
	require.True(t, ok)
	assert.Equal(t, child.ID, got.ID)

	_, _, err := rt.AttachLiveSession(t.Context(), "missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLiveSessionUnavailable)
}

func testLiveControlRuntime(t *testing.T, childID string) (*LocalRuntime, *session.Session, *session.Session) {
	t.Helper()
	root := session.New(session.WithID("root-session"))
	child := session.NewRuntimeManagedSubSession(root, session.WithID(childID), session.WithAgentName("reviewer"))
	rt := &LocalRuntime{
		team:          team.New(team.WithAgents(agent.New("reviewer", "review"))),
		eventBus:      NewEventBus(),
		liveSessions:  newLiveSessionRegistry(),
		subagents:     &SubagentManager{all: make(map[string]*subagentHandle)},
		childQueues:   make(map[string]sessionQueues),
		steerQueue:    NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		now:           time.Now,
	}
	rt.subagents.r = rt
	rt.liveSessions.register(root.ID, "root", "")
	rt.liveSessions.register(child.ID, "reviewer", root.ID)
	rt.subagents.all[child.ID] = &subagentHandle{
		id:        child.ID,
		shortID:   shortID(child.ID),
		agentName: "reviewer",
		parent:    root,
		sess:      child,
		created:   time.Now(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		state:     "running",
	}
	if strings.HasPrefix(childID, "abcde") {
		rt.subagents.all[child.ID].shortID = "abcde"
	}
	return rt, root, child
}
