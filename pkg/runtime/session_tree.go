package runtime

import "sort"

// SessionTree is a structured, queryable wrapper around []LiveSessionNode
// that provides canonical tree traversal and lookup.
type SessionTree struct {
	root  LiveSessionNode
	nodes map[string]LiveSessionNode // sessionID → node
	kids  map[string][]string        // parentID → child session IDs, sorted by CreatedAt
}

// NewSessionTree builds a SessionTree from the given root and descendant
// nodes. It indexes all nodes by session ID and builds a children map
// sorted by CreatedAt. Nil or empty nodes are handled gracefully.
func NewSessionTree(root LiveSessionNode, nodes []LiveSessionNode) *SessionTree {
	t := &SessionTree{
		root:  root,
		nodes: make(map[string]LiveSessionNode, 1+len(nodes)),
		kids:  make(map[string][]string),
	}
	if root.SessionID != "" {
		t.nodes[root.SessionID] = root
	}
	for _, n := range nodes {
		if n.SessionID == "" {
			continue
		}
		t.nodes[n.SessionID] = n
	}
	// Build kids map: for every non-root node, register it under its parent.
	for _, n := range t.nodes {
		if n.SessionID == root.SessionID || n.ParentSessionID == "" {
			continue
		}
		t.kids[n.ParentSessionID] = append(t.kids[n.ParentSessionID], n.SessionID)
	}
	// Sort each child list by CreatedAt.
	for parentID, childIDs := range t.kids {
		sort.Slice(childIDs, func(i, j int) bool {
			ni := t.nodes[childIDs[i]]
			nj := t.nodes[childIDs[j]]
			return ni.CreatedAt.Before(nj.CreatedAt)
		})
		t.kids[parentID] = childIDs
	}
	return t
}

// Root returns the tree's root node.
func (t *SessionTree) Root() LiveSessionNode {
	return t.root
}

// Children returns the direct children of the given parent, sorted by
// CreatedAt. Returns nil when the parent has no children.
func (t *SessionTree) Children(parentID string) []LiveSessionNode {
	ids := t.kids[parentID]
	if len(ids) == 0 {
		return nil
	}
	out := make([]LiveSessionNode, 0, len(ids))
	for _, id := range ids {
		out = append(out, t.nodes[id])
	}
	return out
}

// Node returns the node with the given session ID, or ok=false.
func (t *SessionTree) Node(sessionID string) (LiveSessionNode, bool) {
	n, ok := t.nodes[sessionID]
	return n, ok
}

// Walk performs a depth-first traversal of the tree, calling fn for each
// node. The depth parameter starts at 0 for the root.
func (t *SessionTree) Walk(fn func(node LiveSessionNode, depth int)) {
	t.walk(t.root.SessionID, 0, fn)
}

func (t *SessionTree) walk(id string, depth int, fn func(LiveSessionNode, int)) {
	node, ok := t.nodes[id]
	if !ok {
		return
	}
	fn(node, depth)
	for _, childID := range t.kids[id] {
		t.walk(childID, depth+1, fn)
	}
}

// Slice returns all nodes in flat DFS order.
func (t *SessionTree) Slice() []LiveSessionNode {
	out := make([]LiveSessionNode, 0, len(t.nodes))
	t.Walk(func(node LiveSessionNode, _ int) {
		out = append(out, node)
	})
	return out
}
