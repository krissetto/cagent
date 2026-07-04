package subagent

import (
	"errors"
	"slices"
	"sync"
	"time"
)

var ErrNodeNotFound = errors.New("subagent node not found")

type nodeRecord struct {
	node Node
}

// Tree is a thread-safe registry of running subagent nodes. It is intentionally
// small: state changes update nodes and publish snapshots to subscribers.
type Tree struct {
	mu          sync.RWMutex
	root        NodeID
	nodes       map[NodeID]*nodeRecord
	children    map[NodeID][]NodeID
	subscribers map[chan Snapshot]struct{}
	now         func() time.Time
}

// NewTree returns an empty tree.
func NewTree() *Tree {
	return &Tree{
		nodes:       map[NodeID]*nodeRecord{},
		children:    map[NodeID][]NodeID{},
		subscribers: map[chan Snapshot]struct{}{},
		now:         time.Now,
	}
}

// Add inserts a node. The first parentless node becomes the tree root.
func (t *Tree) Add(n Node) error {
	if n.ID == "" {
		return errors.New("node id is required")
	}
	if n.Agent == "" {
		return errors.New("agent name is required")
	}
	when := t.now()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = when
	}
	n.UpdatedAt = when
	if n.State == "" {
		n.State = NodeStarting
	}

	t.mu.Lock()
	if _, exists := t.nodes[n.ID]; exists {
		t.mu.Unlock()
		return errors.New("node already exists")
	}
	t.nodes[n.ID] = &nodeRecord{node: n}
	if n.Parent == "" && t.root == "" {
		t.root = n.ID
	} else if n.Parent != "" {
		t.children[n.Parent] = append(t.children[n.Parent], n.ID)
	}
	subs := t.subscribersLocked()
	snap := t.snapshotLocked()
	t.mu.Unlock()

	publishSnapshot(subs, snap)
	return nil
}

// Update mutates a node and publishes a new snapshot.
func (t *Tree) Update(id NodeID, fn func(*Node)) error {
	t.mu.Lock()
	rec, ok := t.nodes[id]
	if !ok {
		t.mu.Unlock()
		return ErrNodeNotFound
	}
	fn(&rec.node)
	rec.node.UpdatedAt = t.now()
	subs := t.subscribersLocked()
	snap := t.snapshotLocked()
	t.mu.Unlock()

	publishSnapshot(subs, snap)
	return nil
}

// Node returns a copy of the requested node.
func (t *Tree) Node(id NodeID) (Node, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rec, ok := t.nodes[id]
	if !ok {
		return Node{}, false
	}
	return rec.node, true
}

// NewNodeID mints a fresh short id that is not currently present in the tree,
// retrying on the rare collision.
func (t *Tree) NewNodeID() NodeID {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		id := NewID()
		if _, exists := t.nodes[id]; !exists {
			return id
		}
	}
}

// Snapshot returns a stable, serialisable view of the tree.
func (t *Tree) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snapshotLocked()
}

// Subscribe registers for live tree snapshots. The returned channel receives
// the current snapshot immediately and then future updates. The caller must
// call the returned cancel function.
func (t *Tree) Subscribe(buffer int) (<-chan Snapshot, func()) {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan Snapshot, buffer)
	snap := t.register(ch)
	publishSnapshot([]chan Snapshot{ch}, snap)
	return ch, func() { t.unregister(ch) }
}

func (t *Tree) register(ch chan Snapshot) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subscribers[ch] = struct{}{}
	return t.snapshotLocked()
}

func (t *Tree) unregister(ch chan Snapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.subscribers[ch]; ok {
		delete(t.subscribers, ch)
		close(ch)
	}
}

func (t *Tree) snapshotLocked() Snapshot {
	if t.root != "" {
		return Snapshot{Root: t.root, Nodes: []NodeSnapshot{t.snapshotNodeLocked(t.root)}}
	}
	roots := make([]NodeID, 0, len(t.nodes))
	for id, rec := range t.nodes {
		if rec.node.Parent == "" {
			roots = append(roots, id)
		}
	}
	slices.Sort(roots)
	s := Snapshot{}
	for _, id := range roots {
		s.Nodes = append(s.Nodes, t.snapshotNodeLocked(id))
	}
	return s
}

func (t *Tree) snapshotNodeLocked(id NodeID) NodeSnapshot {
	rec := t.nodes[id]
	snap := NodeSnapshot{Node: rec.node}
	for _, childID := range t.children[id] {
		if _, ok := t.nodes[childID]; ok {
			snap.Children = append(snap.Children, t.snapshotNodeLocked(childID))
		}
	}
	return snap
}

func (t *Tree) subscribersLocked() []chan Snapshot {
	subs := make([]chan Snapshot, 0, len(t.subscribers))
	for ch := range t.subscribers {
		subs = append(subs, ch)
	}
	return subs
}

func publishSnapshot(subs []chan Snapshot, snap Snapshot) {
	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snap:
			default:
			}
		}
	}
}
