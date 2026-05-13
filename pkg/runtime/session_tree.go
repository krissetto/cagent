package runtime

import (
	"sort"
	"time"
)

// LiveSessionNode is a snapshot entry describing a session in the live
// runtime tree. It carries only serializable, stable fields and is
// intentionally client-oriented so server/API layers can return it as-is.
type LiveSessionNode struct {
	ID            string              `json:"id"`
	ParentID      string              `json:"parent_id,omitempty"`
	AgentName     string              `json:"agent_name,omitempty"`
	Status        string              `json:"status"`
	Title         string              `json:"title,omitempty"`
	Kind          LiveSessionNodeKind `json:"kind,omitempty"`
	Depth         int                 `json:"depth"`
	RootSessionID string              `json:"root_session_id,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	LastUpdateAt  time.Time           `json:"last_update_at,omitzero"`
	LastPreview   string              `json:"last_preview,omitempty"`
	Error         string              `json:"error,omitempty"`
	Children      []string            `json:"children,omitempty"` // child session IDs, populated by SessionTree
}

// LiveSessionNodeKind distinguishes root sessions from subagent sessions.
type LiveSessionNodeKind string

const (
	LiveSessionRoot     LiveSessionNodeKind = "root"
	LiveSessionSubAgent LiveSessionNodeKind = "subagent"
)

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
	if root.ID != "" {
		t.nodes[root.ID] = root
	}
	for _, n := range nodes {
		if n.ID == "" {
			continue
		}
		t.nodes[n.ID] = n
	}
	// Build kids map: for every non-root node, register it under its parent.
	for _, n := range t.nodes {
		if n.ID == root.ID || n.ParentID == "" {
			continue
		}
		t.kids[n.ParentID] = append(t.kids[n.ParentID], n.ID)
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

// Slice returns all nodes in flat DFS order. Each node's Children field is
// populated with its direct child IDs for convenience.
func (t *SessionTree) Slice() []LiveSessionNode {
	out := make([]LiveSessionNode, 0, len(t.nodes))
	t.Walk(func(node LiveSessionNode, _ int) {
		node.Children = t.kids[node.ID]
		out = append(out, node)
	})
	return out
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
	t.walk(t.root.ID, 0, fn)
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
