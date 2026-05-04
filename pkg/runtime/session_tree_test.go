package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeNode(id, parentID string, createdAt time.Time) LiveSessionNode {
	kind := LiveSessionSubAgent
	if parentID == "" {
		kind = LiveSessionRoot
	}
	return LiveSessionNode{
		SessionID:       id,
		ParentSessionID: parentID,
		RootSessionID:   rootIDOf(id, parentID),
		Kind:            kind,
		Status:          "running",
		CreatedAt:       createdAt,
	}
}

func rootIDOf(id, parentID string) string {
	if parentID == "" {
		return id
	}
	return parentID
}

var t0 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// TestSessionTree_Empty verifies that a tree built from an empty root and nil
// nodes can be queried without panicking.
func TestSessionTree_Empty(t *testing.T) {
	t.Parallel()
	tree := NewSessionTree(LiveSessionNode{}, nil)
	assert.Equal(t, LiveSessionNode{}, tree.Root())
	assert.Nil(t, tree.Children("anything"))
	_, ok := tree.Node("anything")
	assert.False(t, ok)
	assert.Empty(t, tree.Slice())
}

// TestSessionTree_RootOnly verifies a tree with just a root node.
func TestSessionTree_RootOnly(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	tree := NewSessionTree(root, nil)

	assert.Equal(t, root, tree.Root())
	n, ok := tree.Node("root-1")
	require.True(t, ok)
	assert.Equal(t, root, n)

	assert.Nil(t, tree.Children("root-1"))

	slice := tree.Slice()
	require.Len(t, slice, 1)
	assert.Equal(t, root, slice[0])
}

// TestSessionTree_RootWithChildren verifies ordering of children by CreatedAt.
func TestSessionTree_RootWithChildren(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	// B was created before A; they should come out sorted oldest→newest.
	childB := makeNode("child-b", "root-1", t0.Add(2*time.Minute))
	childA := makeNode("child-a", "root-1", t0.Add(1*time.Minute))

	tree := NewSessionTree(root, []LiveSessionNode{childB, childA})

	kids := tree.Children("root-1")
	require.Len(t, kids, 2)
	assert.Equal(t, "child-a", kids[0].SessionID, "should be sorted by CreatedAt ascending")
	assert.Equal(t, "child-b", kids[1].SessionID)
}

// TestSessionTree_NestedWalkOrderAndDepths verifies DFS walk order and depth
// values for a root → A → B tree.
func TestSessionTree_NestedWalkOrderAndDepths(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	nodeA := makeNode("node-a", "root-1", t0.Add(time.Minute))
	nodeB := makeNode("node-b", "node-a", t0.Add(2*time.Minute))

	tree := NewSessionTree(root, []LiveSessionNode{nodeA, nodeB})

	type result struct {
		id    string
		depth int
	}
	var got []result
	tree.Walk(func(node LiveSessionNode, depth int) {
		got = append(got, result{node.SessionID, depth})
	})

	require.Len(t, got, 3)
	assert.Equal(t, result{"root-1", 0}, got[0])
	assert.Equal(t, result{"node-a", 1}, got[1])
	assert.Equal(t, result{"node-b", 2}, got[2])
}

// TestSessionTree_SliceIsFlattened verifies Slice returns nodes in DFS order.
func TestSessionTree_SliceIsFlattened(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	nodeA := makeNode("node-a", "root-1", t0.Add(time.Minute))
	nodeB := makeNode("node-b", "node-a", t0.Add(2*time.Minute))

	tree := NewSessionTree(root, []LiveSessionNode{nodeA, nodeB})
	slice := tree.Slice()

	require.Len(t, slice, 3)
	assert.Equal(t, "root-1", slice[0].SessionID)
	assert.Equal(t, "node-a", slice[1].SessionID)
	assert.Equal(t, "node-b", slice[2].SessionID)
}

// TestSessionTree_NodeLookup verifies Node hit/miss.
func TestSessionTree_NodeLookup(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	child := makeNode("child-1", "root-1", t0.Add(time.Minute))
	tree := NewSessionTree(root, []LiveSessionNode{child})

	n, ok := tree.Node("root-1")
	require.True(t, ok)
	assert.Equal(t, "root-1", n.SessionID)

	n, ok = tree.Node("child-1")
	require.True(t, ok)
	assert.Equal(t, "child-1", n.SessionID)

	_, ok = tree.Node("missing")
	assert.False(t, ok)
}

// TestSessionTree_ChildrenRootVsLeaf verifies Children for a root (has kids)
// and a leaf (no kids).
func TestSessionTree_ChildrenRootVsLeaf(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	child := makeNode("child-1", "root-1", t0.Add(time.Minute))
	tree := NewSessionTree(root, []LiveSessionNode{child})

	rootKids := tree.Children("root-1")
	require.Len(t, rootKids, 1)
	assert.Equal(t, "child-1", rootKids[0].SessionID)

	leafKids := tree.Children("child-1")
	assert.Nil(t, leafKids)
}

// TestSessionTree_NodesWithEmptyIDAreIgnored verifies that nodes with empty
// session IDs are gracefully skipped.
func TestSessionTree_NodesWithEmptyIDAreIgnored(t *testing.T) {
	t.Parallel()
	root := makeNode("root-1", "", t0)
	blank := LiveSessionNode{SessionID: "", ParentSessionID: "root-1"}
	tree := NewSessionTree(root, []LiveSessionNode{blank})

	// Only the root should be in the tree.
	assert.Len(t, tree.Slice(), 1)
	assert.Nil(t, tree.Children("root-1"),
		"the blank node must not appear as a child")
}
