package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestLiveSessionTreeIncludesDepth3MetadataAndLiveState(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(
		session.WithID("root"),
		session.WithTitle("Root title"),
		session.WithUserMessage("root prompt"),
	)
	reviewer := agent.New("reviewer", "review work")
	rt := &LocalRuntime{
		sessionStore: store,
		liveSessions: newLiveSessionRegistry(),
		team:         team.New(team.WithAgents(reviewer)),
		now:          func() time.Time { return time.Unix(123, 0) },
	}
	require.NoError(t, store.AddSession(ctx, root))

	child1 := mustAddRuntimeManagedChild(t, ctx, store, root, "child1", "reviewer", "first child response")
	child1.Title = "Child one"
	require.NoError(t, store.UpdateSession(ctx, child1))
	child2 := mustAddRuntimeManagedChild(t, ctx, store, child1, "child2", "reviewer", "second child response")
	mustAddRuntimeManagedChild(t, ctx, store, child2, "child3", "reviewer", "third child response")
	rt.liveSessions.register(root.ID, "root", "")
	rt.liveSessions.register(child2.ID, "reviewer", child1.ID)

	tree, err := rt.LiveSessionTree(ctx, root.ID)
	require.NoError(t, err)
	require.NotNil(t, tree.Root)
	assert.Equal(t, "root", tree.Root.ID)
	assert.Equal(t, "Root title", tree.Root.Title)
	assert.Equal(t, 0, tree.Root.Depth)
	assert.True(t, tree.Root.Live)
	require.Len(t, tree.Root.Children, 1)

	gotChild1 := tree.Root.Children[0]
	assert.Equal(t, "child1", gotChild1.ID)
	assert.Equal(t, "reviewer", gotChild1.AgentName)
	assert.Equal(t, "Child one", gotChild1.Title)
	assert.Equal(t, 1, gotChild1.Depth)
	assert.Equal(t, "first child response", gotChild1.Preview)
	assert.Equal(t, gotChild1.Preview, gotChild1.LastPreview)
	assert.False(t, gotChild1.CreatedAt.IsZero())
	assert.False(t, gotChild1.Live)
	require.Len(t, gotChild1.Children, 1)

	gotChild2 := gotChild1.Children[0]
	assert.Equal(t, 2, gotChild2.Depth)
	assert.True(t, gotChild2.Live)
	require.Len(t, gotChild2.Children, 1)

	gotChild3 := gotChild2.Children[0]
	assert.Equal(t, "child3", gotChild3.ID)
	assert.Equal(t, 3, gotChild3.Depth)
	assert.Equal(t, "third child response", gotChild3.LastPreview)
}

func TestSubagentIdleAutoFinalizeTTLUsesPerAgentFallback(t *testing.T) {
	reviewer := agent.New("reviewer", "review work", agent.WithIdleAutoFinalizeTimeout(42*time.Millisecond))
	assert.Equal(t, 42*time.Millisecond, subagentIdleAutoFinalizeTTL(reviewer, 0))

	plain := agent.New("plain", "plain work")
	assert.Equal(t, DefaultSubagentTTL, subagentIdleAutoFinalizeTTL(plain, 0))
	assert.Equal(t, 15*time.Minute, subagentIdleAutoFinalizeTTL(plain, 15*time.Minute))
}

func mustAddRuntimeManagedChild(t *testing.T, ctx context.Context, store session.Store, parent *session.Session, id, agentName, response string) *session.Session {
	t.Helper()
	child := session.NewRuntimeManagedSubSession(parent, session.WithID(id), session.WithAgentName(agentName))
	child.AddMessage(session.NewAgentMessage(agentName, &chat.Message{Role: chat.MessageRoleAssistant, Content: response}))
	require.NoError(t, store.AddSubSession(ctx, parent.ID, child))
	return child
}
