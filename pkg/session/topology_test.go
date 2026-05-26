package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeManagedSubSessionTopology(t *testing.T) {
	root := New(WithID("root"))
	child := NewRuntimeManagedSubSession(root, WithID("child"))
	nested := NewRuntimeManagedSubSession(child, WithID("nested"))

	assert.Equal(t, "root", root.RootID)
	assert.False(t, root.RuntimeManaged)

	assert.Equal(t, "root", child.RootID)
	assert.Equal(t, "root", child.ParentID)
	assert.True(t, child.RuntimeManaged)

	assert.Equal(t, "root", nested.RootID)
	assert.Equal(t, "child", nested.ParentID)
	assert.True(t, nested.RuntimeManaged)
}

func TestRuntimeManagedSubSessionNilParentPanics(t *testing.T) {
	assert.Panics(t, func() {
		NewRuntimeManagedSubSession(nil, WithID("child"))
	})
}

func TestStoreRuntimeTopologyInMemory(t *testing.T) {
	testStoreRuntimeTopology(t, NewInMemorySessionStore())
}

func TestStoreRuntimeTopologySQLite(t *testing.T) {
	testStoreRuntimeTopology(t, openMemoryStore(t))
}

func testStoreRuntimeTopology(t *testing.T, store Store) {
	t.Helper()
	ctx := t.Context()

	root := New(WithID("root"), WithTitle("root title"))
	child := NewRuntimeManagedSubSession(root, WithID("child"), WithTitle("child title"))
	nested := NewRuntimeManagedSubSession(child, WithID("nested"), WithTitle("nested title"))

	require.NoError(t, store.AddSession(ctx, root))
	require.NoError(t, store.AddSubSession(ctx, root.ID, child))
	require.NoError(t, store.AddSubSession(ctx, child.ID, nested))

	gotRoot, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", gotRoot.RootID)
	assert.False(t, gotRoot.RuntimeManaged)

	gotChild, err := store.GetSession(ctx, child.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", gotChild.RootID)
	assert.Equal(t, "root", gotChild.ParentID)
	assert.True(t, gotChild.RuntimeManaged)

	gotNested, err := store.GetSession(ctx, nested.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", gotNested.RootID)
	assert.Equal(t, "child", gotNested.ParentID)
	assert.True(t, gotNested.RuntimeManaged)

	children, err := store.GetChildSessions(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, "child", children[0].ID)

	tree, err := store.GetSessionTree(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, tree, 3)
	assert.ElementsMatch(t, []string{"root", "child", "nested"}, []string{tree[0].ID, tree[1].ID, tree[2].ID})

	resolved, err := store.ResolveRootID(ctx, nested.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", resolved)
}
