package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	MaxSubagentDepth       = 8
	MaxSubagentDescendants = 64
)

type SubagentManager struct {
	r   *LocalRuntime
	mu  sync.Mutex
	all map[string]*subagentHandle
}

type subagentHandle struct {
	id        string
	shortID   string
	agentName string
	parent    *session.Session
	sess      *session.Session
	created   time.Time
	inbox     chan string
	stop      chan struct{}
	done      chan struct{}
	wake      chan struct{}
	mu        sync.Mutex
	state     string
	last      []Event
	envelopes []SubagentEnvelope
}

func NewSubagentManager(r *LocalRuntime) *SubagentManager {
	return &SubagentManager{r: r, all: make(map[string]*subagentHandle)}
}

func (m *SubagentManager) Start(ctx context.Context, parent *session.Session, agentName, task string) (*subagentHandle, error) {
	return m.start(ctx, parent, agentName, task, nil)
}

func (m *SubagentManager) StartWithSink(ctx context.Context, parent *session.Session, agentName, task string, events EventSink) (*subagentHandle, error) {
	return m.start(ctx, parent, agentName, task, events)
}

func (m *SubagentManager) start(ctx context.Context, parent *session.Session, agentName, task string, events EventSink) (*subagentHandle, error) {
	if m == nil || parent == nil {
		return nil, errors.New("subagent manager unavailable")
	}
	agentName = strings.TrimSpace(agentName)
	task = strings.TrimSpace(task)
	if agentName == "" || task == "" {
		return nil, errors.New("agent and task are required")
	}
	if m.depth(parent) >= MaxSubagentDepth {
		return nil, fmt.Errorf("subagent depth cap %d exceeded", MaxSubagentDepth)
	}
	if m.descendants(parent.EffectiveRootID()) >= MaxSubagentDescendants {
		return nil, fmt.Errorf("subagent descendant cap %d exceeded", MaxSubagentDescendants)
	}
	subAgent, selectedSpec, ok := m.r.CurrentAgent().SubAgentForName(agentName)
	if !ok || subAgent == nil {
		return nil, fmt.Errorf("agent %q is not in the subagents list", agentName)
	}
	childAgentName := selectedSpec.Agent
	if childAgentName == "" {
		childAgentName = agentName
	}
	child := session.NewRuntimeManagedSubSession(parent,
		session.WithAgentName(childAgentName),
		session.WithWorkingDir(parent.WorkingDir),
		session.WithToolsApproved(parent.ToolsApproved),
		session.WithExcludedTools(parent.ExcludedTools),
		session.WithAttachedFiles(parent.AttachedFiles),
	)
	child.Permissions = parent.Permissions
	if m.r.sessionStore != nil {
		if err := m.r.sessionStore.AddSubSession(ctx, parent.ID, child); err != nil {
			return nil, err
		}
	} else {
		parent.AddSubSession(child)
	}
	h := &subagentHandle{
		id:        child.ID,
		shortID:   shortID(child.ID),
		agentName: agentName,
		parent:    parent,
		sess:      child,
		created:   m.r.now(),
		inbox:     make(chan string, 64),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		state:     "running",
	}
	m.mu.Lock()
	m.all[h.id] = h
	m.mu.Unlock()
	if m.r.liveSessions != nil {
		m.r.liveSessions.register(child.ID, childAgentName, parent.ID)
	}
	m.publishToParent(parent.ID, SubAgentStarted(h.info(), parent.ID))
	m.emitToSink(events, SubAgentStarted(h.info(), parent.ID))
	m.publishLiveSessionTreeChanged(ctx, child.ID)
	m.emitLiveSessionTreeChanged(ctx, events, child.ID)
	h.inbox <- task
	go m.runChild(ctx, h)
	return h, nil
}

func (m *SubagentManager) Send(parent *session.Session, ref, msg string) error {
	h, err := m.resolve(parent, ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(msg) == "" {
		return errors.New("message cannot be empty")
	}
	select {
	case h.inbox <- msg:
		h.mu.Lock()
		h.state = "running"
		h.mu.Unlock()
		m.publishToParent(parent.ID, SubAgentSent(h.id, msg, parent.ID))
		m.publishLiveSessionTreeChanged(context.Background(), h.id)
		h.signal()
		return nil
	case <-h.done:
		return errors.New("subagent stopped")
	}
}

func (m *SubagentManager) Stop(parent *session.Session, ref string) error {
	h, err := m.resolve(parent, ref)
	if err != nil {
		return err
	}
	select {
	case <-h.stop:
	default:
		h.mu.Lock()
		h.state = "stopped"
		h.mu.Unlock()
		close(h.stop)
		m.publishToParent(parent.ID, SubAgentUpdate(h.envelope("stopped", "stopped", ""), parent.ID))
		m.publishLiveSessionTreeChanged(context.Background(), h.id)
	}
	return nil
}

func (m *SubagentManager) Finalize(parent *session.Session, ref string) error {
	h, err := m.resolve(parent, ref)
	if err != nil {
		return err
	}
	select {
	case <-h.stop:
	default:
		h.mu.Lock()
		h.state = "closed"
		h.mu.Unlock()
		close(h.stop)
		m.publishToParent(parent.ID, SubAgentUpdate(h.envelope("closed", "closed", ""), parent.ID))
		m.publishLiveSessionTreeChanged(context.Background(), h.id)
	}
	return nil
}

func (m *SubagentManager) List(parent *session.Session) []SubagentInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SubagentInfo
	root := parent.EffectiveRootID()
	for _, h := range m.all {
		if h.sess.EffectiveRootID() == root && h.parent.ID == parent.ID && h.live() {
			out = append(out, h.info())
		}
	}
	return out
}

func (m *SubagentManager) Inspect(parent *session.Session, ref, mode string) (string, error) {
	h, err := m.resolve(parent, ref)
	if err != nil {
		return "", err
	}
	items := h.sess.Messages
	var limit int
	switch mode {
	case "", "last":
		limit = 1
	case "recent":
		limit = 6
	case "full":
		limit = len(items)
	default:
		return "", errors.New("mode must be last, recent, or full")
	}
	if len(items) < limit {
		limit = len(items)
	}
	var b strings.Builder
	for _, item := range items[len(items)-limit:] {
		if item.Message != nil {
			fmt.Fprintf(&b, "%s: %s\n", item.Message.Message.Role, truncateEnvelope(item.Message.Message.Content, 4000))
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func (m *SubagentManager) DrainEnvelopes(ctx context.Context, parent *session.Session, events EventSink) bool {
	if m == nil || parent == nil {
		return false
	}
	var drained bool
	for _, h := range m.children(parent) {
		h.mu.Lock()
		envs := append([]SubagentEnvelope(nil), h.envelopes...)
		h.envelopes = nil
		h.mu.Unlock()
		for _, env := range envs {
			text := env.parentText()
			msg := session.SubagentEnvelopeMessage(text)
			pos := len(parent.Messages)
			if m.r.sessionStore != nil {
				if ps, ok := m.r.sessionStore.(session.PositionalStore); ok {
					_, _ = ps.AddMessageAt(ctx, parent.ID, pos, msg)
				} else {
					_, _ = m.r.sessionStore.AddMessage(ctx, parent.ID, msg)
				}
			}
			parent.AddMessage(msg)
			events.Emit(TypedUserMessage(session.MessageKindSubagentEnvelope, text, parent.ID, nil, len(parent.Messages)-1))
			drained = true
		}
	}
	return drained
}

func (m *SubagentManager) WaitForSubagentWork(ctx context.Context, parent *session.Session, queues sessionQueues, events EventSink) bool {
	children := m.liveChildren(parent)
	if len(children) == 0 {
		return false
	}
	ids := make([]string, 0, len(children))
	for _, h := range children {
		ids = append(ids, h.shortID)
	}
	events.Emit(ParentIdle(parent.ID, len(ids), ids))
	defer events.Emit(ParentResume(parent.ID, len(ids), ids))
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if m.DrainEnvelopes(ctx, parent, events) {
			return true
		}
		if queueHasPending(queues.steer) || queueHasPending(queues.followUp) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		case <-m.anyWake(children):
		}
	}
}

func (m *SubagentManager) runChild(ctx context.Context, h *subagentHandle) {
	defer close(h.done)
	defer func() {
		h.mu.Lock()
		if h.state != "closed" && h.state != "failed" {
			h.state = "stopped"
		}
		h.mu.Unlock()
		h.signal()
		if m.r.liveSessions != nil {
			m.r.liveSessions.unregister(h.id)
		}
		if m.r.eventBus != nil {
			m.r.eventBus.CloseTopic(h.id)
		}
		m.publishLiveSessionTreeChanged(context.Background(), h.id)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case prompt := <-h.inbox:
			h.runPrompt(ctx, m, prompt)
		case <-h.wake:
			if prompt, ok := m.dequeueChildPrompt(ctx, h); ok {
				h.runPrompt(ctx, m, prompt)
			}
		}
	}
}

func (h *subagentHandle) runPrompt(ctx context.Context, m *SubagentManager, prompt string) {
	m.maybeGenerateChildTitle(ctx, h, prompt)
	h.mu.Lock()
	h.state = "running"
	h.mu.Unlock()
	m.publishToParent(h.parent.ID, SubAgentUpdate(h.envelope("status_only", "running", ""), h.parent.ID))
	m.publishLiveSessionTreeChanged(ctx, h.id)
	h.sess.AddMessage(session.UserMessage(prompt))
	stream := m.r.RunStream(ctx, h.sess)
	for ev := range stream {
		h.appendEvent(ev)
	}
	// Drain any queued child inbox before notifying the parent.
	if len(h.inbox) > 0 {
		return
	}
	preview := h.latestPreview()
	m.enqueueEnvelope(h, h.envelope("turn_completed", "waiting", preview))
	h.mu.Lock()
	h.state = "waiting"
	h.mu.Unlock()
	m.publishLiveSessionTreeChanged(ctx, h.id)
}

func (m *SubagentManager) dequeueChildPrompt(ctx context.Context, h *subagentHandle) (string, bool) {
	queues := m.r.queuesFor(h.sess)
	if msg, ok := queues.steer.Dequeue(ctx); ok {
		return queuedMessageContent(msg), true
	}
	if msg, ok := queues.followUp.Dequeue(ctx); ok {
		m.r.publishQueueSnapshot(h.id, queues.followUp)
		return queuedMessageContent(msg), true
	}
	return "", false
}

func queuedMessageContent(msg QueuedMessage) string {
	if msg.Content != "" {
		return msg.Content
	}
	for _, part := range msg.MultiContent {
		if part.Type == chat.MessagePartTypeText && part.Text != "" {
			return part.Text
		}
	}
	return ""
}

func (m *SubagentManager) enqueueEnvelope(h *subagentHandle, env SubagentEnvelope) {
	h.mu.Lock()
	h.envelopes = append(h.envelopes, env)
	h.mu.Unlock()
	m.publishToParent(h.parent.ID, SubAgentUpdate(env, h.parent.ID))
	h.signal()
}

func (m *SubagentManager) maybeGenerateChildTitle(ctx context.Context, h *subagentHandle, prompt string) {
	if m == nil || m.r == nil || h == nil || h.sess == nil || h.sess.Title != "" {
		return
	}
	gen := m.r.titleGeneratorForSession(h.sess)
	if gen == nil {
		return
	}

	m.r.titleGenMu.Lock()
	if m.r.titleGen == nil {
		m.r.titleGen = make(map[string]bool)
	}
	if m.r.titleGen[h.id] {
		m.r.titleGenMu.Unlock()
		return
	}
	m.r.titleGen[h.id] = true
	m.r.titleGenMu.Unlock()

	go m.generateChildTitle(ctx, h, gen, []string{prompt})
}

func (m *SubagentManager) generateChildTitle(ctx context.Context, h *subagentHandle, gen *sessiontitle.Generator, userMessages []string) {
	defer func() {
		m.r.titleGenMu.Lock()
		delete(m.r.titleGen, h.id)
		m.r.titleGenMu.Unlock()
	}()

	title, err := gen.Generate(ctx, h.id, userMessages)
	if err != nil || title == "" {
		return
	}
	if err := m.r.UpdateSessionTitle(ctx, h.sess, title); err != nil {
		return
	}
	ev := SessionTitle(h.id, title)
	if m.r.eventBus != nil {
		m.r.eventBus.Publish(h.id, ev)
	}
	m.publishLiveSessionTreeChanged(ctx, h.id)
}

func (h *subagentHandle) appendEvent(ev Event) {
	h.mu.Lock()
	h.last = append(h.last, ev)
	if len(h.last) > 20 {
		h.last = h.last[len(h.last)-20:]
	}
	h.mu.Unlock()
}

func (h *subagentHandle) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *subagentHandle) live() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (m *SubagentManager) resolve(parent *session.Session, ref string) (*subagentHandle, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("subagent id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var match *subagentHandle
	root := parent.EffectiveRootID()
	for _, h := range m.all {
		if h.id == ref || strings.HasPrefix(h.id, ref) || h.shortID == ref {
			if h.sess.EffectiveRootID() != root {
				return nil, errors.New("cross-root subagent access rejected")
			}
			if match != nil {
				return nil, errors.New("ambiguous subagent id")
			}
			match = h
		}
	}
	if match == nil {
		return nil, errors.New("subagent not found")
	}
	return match, nil
}

func (m *SubagentManager) ResolveSession(ref string) (*subagentHandle, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("session id required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var match *subagentHandle
	for _, h := range m.all {
		if h.id == ref || strings.HasPrefix(h.id, ref) || h.shortID == ref {
			if match != nil {
				return nil, errors.New("ambiguous session id")
			}
			match = h
		}
	}
	if match == nil {
		return nil, ErrLiveSessionUnavailable
	}
	return match, nil
}

func (m *SubagentManager) children(parent *session.Session) []*subagentHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*subagentHandle
	for _, h := range m.all {
		if h.parent.ID == parent.ID {
			out = append(out, h)
		}
	}
	return out
}

func (m *SubagentManager) liveChildren(parent *session.Session) []*subagentHandle {
	var out []*subagentHandle
	for _, h := range m.children(parent) {
		if h.live() {
			out = append(out, h)
		}
	}
	return out
}

func (m *SubagentManager) descendants(root string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.all {
		if h.sess.EffectiveRootID() == root {
			n++
		}
	}
	return n
}

func queueHasPending(queue MessageQueue) bool {
	if snapshotter, ok := queue.(QueueSnapshotter); ok {
		return len(snapshotter.Snapshot()) > 0
	}
	return false
}

func (m *SubagentManager) depth(s *session.Session) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.depthLocked(s)
}

func (m *SubagentManager) depthLocked(s *session.Session) int {
	d := 0
	id := s.ParentID
	for id != "" {
		d++
		var next string
		for _, h := range m.all {
			if h.id == id {
				next = h.sess.ParentID
				break
			}
		}
		id = next
	}
	return d
}

func (m *SubagentManager) anyWake(children []*subagentHandle) <-chan struct{} {
	ch := make(chan struct{}, 1)
	for _, h := range children {
		go func(h *subagentHandle) {
			select {
			case <-h.wake:
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-h.done:
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}(h)
	}
	return ch
}

type SubagentInfo struct {
	ID        string    `json:"id"`
	ShortID   string    `json:"short_id"`
	AgentName string    `json:"agent_name"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

type SubagentEnvelope struct {
	SubAgentID      string    `json:"subagent_id"`
	ParentSessionID string    `json:"parent_session_id"`
	AgentName       string    `json:"agent_name"`
	Kind            string    `json:"kind"`
	Status          string    `json:"status"`
	Preview         string    `json:"preview,omitempty"`
	Truncated       bool      `json:"truncated,omitempty"`
	Error           string    `json:"error,omitempty"`
	At              time.Time `json:"at"`
}

func (h *subagentHandle) stopNow() error {
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	return nil
}

func (h *subagentHandle) finalize() error {
	return h.stopNow()
}

func (h *subagentHandle) info() SubagentInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return SubagentInfo{ID: h.id, ShortID: h.shortID, AgentName: h.agentName, State: h.state, CreatedAt: h.created}
}

func (h *subagentHandle) envelope(kind, status, preview string) SubagentEnvelope {
	if preview == "" {
		preview = h.latestPreview()
	}
	if status == "" {
		h.mu.Lock()
		status = h.state
		h.mu.Unlock()
	}
	return SubagentEnvelope{
		SubAgentID:      h.id,
		ParentSessionID: h.parent.ID,
		AgentName:       h.agentName,
		Kind:            kind,
		Status:          status,
		Preview:         preview,
		Truncated:       len([]rune(preview)) >= 320,
		At:              time.Now(),
	}
}

func (h *subagentHandle) latestPreview() string {
	for _, item := range slices.Backward(h.sess.OwnMessages()) {
		msg := item.Message
		if msg.Role == chat.MessageRoleSystem || msg.Role == chat.MessageRoleUser {
			continue
		}
		if text := strings.TrimSpace(messageTextPreview(msg)); text != "" {
			return truncateRunes(text, 320)
		}
	}
	return ""
}

func (e SubagentEnvelope) parentText() string {
	ref := fmt.Sprintf("[%s] (%s)", e.AgentName, shortID(e.SubAgentID))
	switch e.Kind {
	case "turn_completed":
		if e.Preview != "" {
			return fmt.Sprintf("%s turn finished. Preview: %s", ref, e.Preview)
		}
		return ref + " turn finished."
	case "closed":
		return ref + " finalized."
	case "stopped":
		return ref + " stopped."
	case "failed":
		if e.Error != "" {
			return fmt.Sprintf("%s failed: %s", ref, e.Error)
		}
		return ref + " failed."
	default:
		return ref + " status: " + e.Status + "."
	}
}

func (m *SubagentManager) emitToSink(events EventSink, ev Event) {
	if events == nil || ev == nil {
		return
	}
	events.Emit(ev)
}

func (m *SubagentManager) publishToParent(parentID string, ev Event) {
	if m == nil || m.r == nil || m.r.eventBus == nil || parentID == "" || ev == nil {
		return
	}
	m.r.eventBus.Publish(parentID, ev)
}

func (m *SubagentManager) emitLiveSessionTreeChanged(ctx context.Context, events EventSink, sessionID string) {
	if events == nil || m == nil || m.r == nil {
		return
	}
	tree, err := m.r.LiveSessionTree(ctx, sessionID)
	if err == nil && tree != nil {
		events.Emit(LiveSessionTreeChanged(sessionID, tree))
		return
	}
	if m.r.liveSessions == nil {
		return
	}
	if info, ok := m.r.liveSessions.get(sessionID); ok && info.ParentID != "" {
		if tree, err := m.r.LiveSessionTree(ctx, info.ParentID); err == nil && tree != nil {
			events.Emit(LiveSessionTreeChanged(info.ParentID, tree))
		}
	}
}

func (m *SubagentManager) publishLiveSessionTreeChanged(ctx context.Context, sessionID string) {
	if m == nil || m.r == nil || m.r.eventBus == nil || sessionID == "" {
		return
	}
	tree, err := m.r.LiveSessionTree(ctx, sessionID)
	if err != nil || tree == nil || tree.Root == nil {
		return
	}
	ev := LiveSessionTreeChanged(sessionID, tree)
	publishTreeChangedRecursive(m.r.eventBus, tree.Root, ev)
}

func publishTreeChangedRecursive(bus *EventBus, node *LiveSessionNode, ev Event) {
	if bus == nil || node == nil || ev == nil {
		return
	}
	bus.Publish(node.ID, ev)
	for _, child := range node.Children {
		publishTreeChangedRecursive(bus, child, ev)
	}
}

func shortID(id string) string {
	if len(id) < 5 {
		return id
	}
	return id[:5]
}

func truncateEnvelope(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func jsonResult(v any) *tools.ToolCallResult {
	b, _ := json.Marshal(v)
	return tools.ResultSuccess(string(b))
}
