package runtime

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

const liveSessionPreviewMaxRunes = 200

// LiveSessionTree is a live-observability representation of a root session and
// its runtime-managed descendants.
type LiveSessionTree struct {
	Root *LiveSessionNode `json:"root"`
}

// LiveSessionNode describes one session in a live session tree.
type LiveSessionNode struct {
	ID          string             `json:"id"`
	ParentID    string             `json:"parent_id,omitempty"`
	AgentName   string             `json:"agent_name,omitempty"`
	Title       string             `json:"title,omitempty"`
	Depth       int                `json:"depth"`
	Preview     string             `json:"preview,omitempty"`
	LastPreview string             `json:"last_preview,omitempty"`
	RootID      string             `json:"root_id,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	Live        bool               `json:"live"`
	Status      string             `json:"status,omitempty"`
	UpdateKind  string             `json:"update_kind,omitempty"`
	Error       string             `json:"error,omitempty"`
	UpdatedAt   time.Time          `json:"updated_at,omitzero"`
	Children    []*LiveSessionNode `json:"children,omitempty"`
}

func (r *LocalRuntime) LiveSessionTree(ctx context.Context, sessionID string) (*LiveSessionTree, error) {
	if r == nil || r.sessionStore == nil {
		return nil, ErrLiveSessionUnavailable
	}

	rootID := sessionID
	if rootID == "" {
		return nil, ErrLiveSessionUnavailable
	}
	treeStore, ok := r.sessionStore.(session.TreeStore)
	if !ok {
		return nil, ErrLiveSessionUnavailable
	}
	sessions, err := treeStore.GetSessionTree(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, ErrLiveSessionUnavailable
	}

	live := r.liveSessionIDs()
	nodes := make(map[string]*LiveSessionNode, len(sessions))
	var root *LiveSessionNode
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		node := liveSessionNodeFromSession(sess, live[sess.ID])
		if r.subagents != nil {
			if h, err := r.subagents.ResolveSession(sess.ID); err == nil && h != nil {
				h.mu.Lock()
				node.AgentName = h.agentName
				node.Status = h.state
				node.LastPreview = h.latestPreview()
				node.UpdatedAt = h.created
				if len(h.envelopes) > 0 {
					last := h.envelopes[len(h.envelopes)-1]
					node.UpdateKind = last.Kind
					node.LastPreview = last.Preview
					node.Error = last.Error
					node.UpdatedAt = last.At
				}
				h.mu.Unlock()
			}
		}
		nodes[sess.ID] = node
		if sess.ID == rootID {
			root = node
		}
	}
	if root == nil {
		return nil, ErrLiveSessionUnavailable
	}

	for _, sess := range sessions {
		if sess == nil || sess.ParentID == "" || sess.ID == rootID {
			continue
		}
		parent := nodes[sess.ParentID]
		child := nodes[sess.ID]
		if parent == nil || child == nil {
			continue
		}
		parent.Children = append(parent.Children, child)
	}
	assignLiveSessionDepth(root, 0)

	return &LiveSessionTree{Root: root}, nil
}

func (r *LocalRuntime) liveSessionIDs() map[string]bool {
	live := map[string]bool{}
	if r.liveSessions == nil {
		return live
	}
	r.liveSessions.mu.RLock()
	defer r.liveSessions.mu.RUnlock()
	for id := range r.liveSessions.sessions {
		live[id] = true
	}
	return live
}

func liveSessionNodeFromSession(sess *session.Session, live bool) *LiveSessionNode {
	preview := liveSessionPreview(sess)
	return &LiveSessionNode{
		ID:          sess.ID,
		ParentID:    sess.ParentID,
		AgentName:   liveSessionAgentName(sess),
		Title:       sess.Title,
		Preview:     preview,
		LastPreview: preview,
		RootID:      sess.EffectiveRootID(),
		CreatedAt:   sess.CreatedAt,
		Live:        live,
		Status:      statusFromLive(live),
		UpdatedAt:   sess.CreatedAt,
	}
}

func statusFromLive(live bool) string {
	if live {
		return "running"
	}
	return "closed"
}

func liveSessionAgentName(sess *session.Session) string {
	if sess.AgentName != "" {
		return sess.AgentName
	}
	for _, message := range slices.Backward(sess.OwnMessages()) {
		if message.AgentName != "" {
			return message.AgentName
		}
	}
	return ""
}

func assignLiveSessionDepth(node *LiveSessionNode, depth int) {
	if node == nil {
		return
	}
	node.Depth = depth
	for _, child := range node.Children {
		assignLiveSessionDepth(child, depth+1)
	}
}

func liveSessionPreview(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	for _, item := range slices.Backward(sess.OwnMessages()) {
		msg := item.Message
		if msg.Role == chat.MessageRoleSystem {
			continue
		}
		if text := strings.TrimSpace(messageTextPreview(msg)); text != "" {
			return truncateRunes(text, liveSessionPreviewMaxRunes)
		}
	}
	return ""
}

func messageTextPreview(msg chat.Message) string {
	if msg.Content != "" {
		return msg.Content
	}
	if len(msg.MultiContent) == 0 {
		return ""
	}
	var b strings.Builder
	for _, part := range msg.MultiContent {
		if part.Type != chat.MessagePartTypeText || part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
