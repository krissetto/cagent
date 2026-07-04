package subagent

import "time"

// NodeID uniquely identifies a running agent instance in a subagent tree.
type NodeID string

// NodeState is the lifecycle of a running agent instance.
type NodeState string

const (
	NodeStarting  NodeState = "starting"
	NodeRunning   NodeState = "running"
	NodeIdle      NodeState = "idle"
	NodeCompleted NodeState = "completed"
	NodeFailed    NodeState = "failed"
	NodeStopped   NodeState = "stopped"
)

// Node represents one running agent instance in the swarm.
type Node struct {
	ID          NodeID    `json:"id"`
	Agent       string    `json:"agent"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Parent      NodeID    `json:"parent,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Task        string    `json:"task,omitempty"`
	State       NodeState `json:"state"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DisplayName returns the model-facing name for the node.
func (n Node) DisplayName() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Agent
}

// NodeSnapshot is a stable, serialisable view of a node and its children.
type NodeSnapshot struct {
	Node     Node           `json:"node"`
	Children []NodeSnapshot `json:"children,omitempty"`
}

// Snapshot is a serialisable view of the whole running tree.
type Snapshot struct {
	Root  NodeID         `json:"root,omitempty"`
	Nodes []NodeSnapshot `json:"nodes,omitempty"`
}
