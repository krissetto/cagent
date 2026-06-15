package sidebar

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestPersistedLiveSessionTreeHydratesMetadataLikeLiveTree(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithTitle("Root"))
	root.CreatedAt = time.Now().Add(-10 * time.Minute)
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child12345"), session.WithTitle("Generated Child Title"), session.WithAgentName("greppy"))
	child.CreatedAt = time.Now().Add(-2 * time.Minute)
	child.SetUsage(1234, 56)
	child.AddMessage(&session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "child latest detail"}})
	grandchild := session.NewRuntimeManagedSubSession(child, session.WithID("grand12345"), session.WithTitle("Generated Grandchild Title"), session.WithAgentName("reviewer"))
	grandchild.CreatedAt = time.Now().Add(-1 * time.Minute)
	grandchild.AddMessage(session.UserMessage("grandchild latest detail"))
	require.NoError(t, store.AddSession(ctx, root))
	require.NoError(t, store.AddSubSession(ctx, "root", child))
	require.NoError(t, store.AddSubSession(ctx, "child12345", grandchild))

	persisted := persistedLiveSessionTree(ctx, root, store)
	require.NotNil(t, persisted)
	require.NotNil(t, persisted.Root)
	require.Len(t, persisted.Root.Children, 1)
	childNode := persisted.Root.Children[0]
	require.Equal(t, "child12345", childNode.ID)
	require.Equal(t, "greppy", childNode.AgentName)
	require.Equal(t, "Generated Child Title", childNode.Title)
	require.Equal(t, "waiting", childNode.Status)
	require.False(t, childNode.Live)
	require.Equal(t, 1, childNode.Depth)
	require.Equal(t, child.CreatedAt, childNode.CreatedAt)
	require.Equal(t, "child latest detail", childNode.Preview)
	require.Equal(t, childNode.Preview, childNode.LastPreview)
	require.Len(t, childNode.Children, 1)
	require.Equal(t, 2, childNode.Children[0].Depth)
	require.Equal(t, "reviewer", childNode.Children[0].AgentName)
	require.Equal(t, "grandchild latest detail", childNode.Children[0].Preview)
}

func TestPersistedSidebarRenderMatchesLiveImportantSubagentMetadata(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithTitle("Root"))
	createdChild := time.Now().Add(-3 * time.Minute)
	createdGrand := time.Now().Add(-2 * time.Minute)
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child12345"), session.WithTitle("Generated Child Title"), session.WithAgentName("greppy"))
	child.CreatedAt = createdChild
	child.AddMessage(&session.Message{AgentName: "greppy", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "child latest detail"}})
	grandchild := session.NewRuntimeManagedSubSession(child, session.WithID("grand12345"), session.WithTitle("Generated Grandchild Title"), session.WithAgentName("reviewer"))
	grandchild.CreatedAt = createdGrand
	grandchild.AddMessage(&session.Message{AgentName: "reviewer", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "grand latest detail"}})
	require.NoError(t, store.AddSession(ctx, root))
	require.NoError(t, store.AddSubSession(ctx, "root", child))
	require.NoError(t, store.AddSubSession(ctx, "child12345", grandchild))

	persistedTree := persistedLiveSessionTree(ctx, root, store)
	liveTree := &runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{ID: "root", Title: "Root", Children: []*runtime.LiveSessionNode{
		{ID: "child12345", ParentID: "root", RootID: "root", AgentName: "greppy", Title: "Generated Child Title", Status: "waiting", Live: true, CreatedAt: createdChild, Preview: "child latest detail", LastPreview: "child latest detail", Depth: 1, Children: []*runtime.LiveSessionNode{
			{ID: "grand12345", ParentID: "child12345", RootID: "root", AgentName: "reviewer", Title: "Generated Grandchild Title", Status: "waiting", Live: true, CreatedAt: createdGrand, Preview: "grand latest detail", LastPreview: "grand latest detail", Depth: 2},
		}},
	}}}

	persistedPlain := sidebarPlainSubagents(t, persistedTree)
	livePlain := sidebarPlainSubagents(t, liveTree)
	for _, want := range []string{"Subagents", "greppy", "child", "idle", "reviewer", "grand", "└ • reviewer"} {
		require.Contains(t, persistedPlain, want)
		require.Contains(t, livePlain, want)
	}
	require.NotContains(t, persistedPlain, "Generated Child Title", "row label must stay agent name + id")
	require.NotContains(t, persistedPlain, "Generated Grandchild Title", "row label must stay agent name + id")
	require.NotContains(t, persistedPlain, "working")
	require.Equal(t, strings.Count(livePlain, "greppy"), strings.Count(persistedPlain, "greppy"))
	require.Equal(t, strings.Count(livePlain, "reviewer"), strings.Count(persistedPlain, "reviewer"))
}

func sidebarPlainSubagents(t *testing.T, tree *runtime.LiveSessionTree) string {
	t.Helper()
	m := New(nil).(*model)
	m.SetLiveSessionTree(tree)
	return strings.Join(stripANSILines(strings.Split(m.subagentsSection(100), "\n")), "\n")
}
