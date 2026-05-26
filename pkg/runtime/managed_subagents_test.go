package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type blockingTestProvider struct {
	id       string
	started  chan struct{}
	release  chan struct{}
	content  string
	toolSeen chan []tools.Tool
}

type queueIsolationProvider struct {
	id       string
	started  chan struct{}
	release  chan struct{}
	seen     chan []chat.Message
	response string
}

func (p *queueIsolationProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero(p.id) }
func (p *queueIsolationProvider) BaseConfig() base.Config { return base.Config{} }
func (p *queueIsolationProvider) MaxTokens() int          { return 0 }
func (p *queueIsolationProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	if p.seen != nil {
		cp := append([]chat.Message(nil), messages...)
		p.seen <- cp
	}
	if p.started != nil {
		close(p.started)
		p.started = nil
	}
	if p.release != nil {
		<-p.release
		p.release = nil
	}
	return newStreamBuilder().AddContent(p.response).AddStopWithUsage(1, 1).Build(), nil
}

func (p *blockingTestProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero(p.id) }
func (p *blockingTestProvider) BaseConfig() base.Config { return base.Config{} }
func (p *blockingTestProvider) MaxTokens() int          { return 0 }
func (p *blockingTestProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, toolList []tools.Tool) (chat.MessageStream, error) {
	if p.toolSeen != nil {
		select {
		case p.toolSeen <- toolList:
		default:
		}
	}
	if p.started != nil {
		close(p.started)
	}
	if p.release != nil {
		<-p.release
	}
	return newStreamBuilder().AddContent(p.content).AddStopWithUsage(1, 1).Build(), nil
}

func newSubagentTestRuntime(t *testing.T, childProvider *blockingTestProvider) (*LocalRuntime, *session.Session) {
	t.Helper()
	worker := agent.New("worker", "worker", agent.WithModel(childProvider))
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/root", stream: newStreamBuilder().AddContent("root").AddStopWithUsage(1, 1).Build()}), agent.WithSubAgents(worker))
	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, worker)), WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return rt, session.New(session.WithID("parent"), session.WithRootID("parent"))
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}

func TestManagedSubagentStartCreatesRuntimeManagedChildAndPublicEvents(t *testing.T) {
	provider := &blockingTestProvider{id: "test/worker", content: "done"}
	rt, parent := newSubagentTestRuntime(t, provider)
	result, err := rt.managedSubagents.handleStart(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"worker","task":"do work"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Eventually(t, func() bool {
		rt.managedSubagents.mu.RLock()
		defer rt.managedSubagents.mu.RUnlock()
		for _, ms := range rt.managedSubagents.items {
			return ms.loadStatus() == managedSubagentStatusCompleted
		}
		return false
	}, time.Second, 10*time.Millisecond)

	children, err := rt.sessionStore.GetChildSessions(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	child := children[0]
	require.Equal(t, parent.ID, child.ParentID)
	require.Equal(t, parent.ID, child.RootID)
	require.True(t, child.RuntimeManaged)
	require.Equal(t, "worker", rt.managedSubagents.items[child.ID].agentName)

	replay, err := ReplayPublicRuntimeEvents(t.Context(), rt.sessionStore, session.PublicRuntimeEventQuery{RootID: parent.ID, SessionID: child.ID})
	require.NoError(t, err)
	require.NotEmpty(t, replay)
	var hasStart, hasChoice, hasStop, hasLifecycleRunning, hasLifecycleCompleted bool
	for _, ev := range replay {
		hasStart = hasStart || ev.Type == "stream_started"
		hasChoice = hasChoice || ev.Type == "agent_choice"
		hasStop = hasStop || ev.Type == "stream_stopped"
		hasLifecycleRunning = hasLifecycleRunning || ev.Type == "subagent_lifecycle" && ev.PayloadJSON != "" && containsAll(ev.PayloadJSON, "running")
		hasLifecycleCompleted = hasLifecycleCompleted || ev.Type == "subagent_lifecycle" && ev.PayloadJSON != "" && containsAll(ev.PayloadJSON, "completed")
	}
	require.True(t, hasStart)
	require.True(t, hasChoice)
	require.True(t, hasStop)
	require.True(t, hasLifecycleRunning)
	require.True(t, hasLifecycleCompleted)
}

func TestManagedSubagentNestedPreservesRoot(t *testing.T) {
	provider := &blockingTestProvider{id: "test/worker", content: "done"}
	rt, parent := newSubagentTestRuntime(t, provider)
	parent.RootID = "root-session"
	result, err := rt.managedSubagents.handleStart(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"worker","task":"nested"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Eventually(t, func() bool {
		children, err := rt.sessionStore.GetChildSessions(t.Context(), parent.ID)
		return err == nil && len(children) == 1
	}, time.Second, 10*time.Millisecond)
	children, err := rt.sessionStore.GetChildSessions(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Equal(t, "root-session", children[0].RootID)
}

func TestManagedSubagentSendInspectListStopErrors(t *testing.T) {
	provider := &blockingTestProvider{id: "test/worker", started: make(chan struct{}), release: make(chan struct{}), content: "done"}
	rt, parent := newSubagentTestRuntime(t, provider)
	_, err := rt.managedSubagents.handleStart(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"worker","task":"wait"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	<-provider.started

	var id string
	rt.managedSubagents.mu.RLock()
	for key := range rt.managedSubagents.items {
		id = key
	}
	rt.managedSubagents.mu.RUnlock()
	require.NotEmpty(t, id)

	result, err := rt.managedSubagents.handleSend(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q,"message":"later"}`, id[:8])}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	msg, ok := rt.managedSubagents.items[id].queue.followUp.Dequeue(t.Context())
	require.True(t, ok)
	require.Equal(t, "later", msg.Content)

	result, err = rt.managedSubagents.handleSend(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q,"message":"now","mode":"steer"}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	msg, ok = rt.managedSubagents.items[id].queue.steer.Dequeue(t.Context())
	require.True(t, ok)
	require.Equal(t, "now", msg.Content)

	result, err = rt.managedSubagents.handleList(t.Context(), parent, tools.ToolCall{}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Contains(t, result.Output, id)

	result, err = rt.managedSubagents.handleInspect(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q,"mode":"last"}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Contains(t, result.Output, "Status: running")

	result, err = rt.managedSubagents.handleStop(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	close(provider.release)
	require.Eventually(t, func() bool { return rt.managedSubagents.items[id].loadStatus() == managedSubagentStatusStopped }, time.Second, 10*time.Millisecond)

	result, err = rt.managedSubagents.handleSend(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q,"message":"late"}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.True(t, result.IsError)

	result, err = rt.managedSubagents.handleInspect(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"subagent_id":"missing"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.True(t, result.IsError)
}

func TestManagedSubagentRejectsCrossRootOperations(t *testing.T) {
	provider := &blockingTestProvider{id: "test/worker", started: make(chan struct{}), release: make(chan struct{}), content: "done"}
	rt, parent := newSubagentTestRuntime(t, provider)
	_, err := rt.managedSubagents.handleStart(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"worker","task":"wait"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	<-provider.started
	var id string
	rt.managedSubagents.mu.RLock()
	for key := range rt.managedSubagents.items {
		id = key
	}
	rt.managedSubagents.mu.RUnlock()

	otherRoot := session.New(session.WithID("other"), session.WithRootID("other"))
	result, err := rt.managedSubagents.handleInspect(t.Context(), otherRoot, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	result, err = rt.managedSubagents.handleStop(t.Context(), otherRoot, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	close(provider.release)
}

func TestManagedSubagentQueueIsolationThroughRunStream(t *testing.T) {
	provider := &queueIsolationProvider{id: "test/worker", started: make(chan struct{}), release: make(chan struct{}), seen: make(chan []chat.Message, 4), response: "done"}
	worker := agent.New("worker", "worker", agent.WithModel(provider))
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/root", stream: newStreamBuilder().AddContent("root").AddStopWithUsage(1, 1).Build()}), agent.WithSubAgents(worker))
	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, worker)), WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	parent := session.New(session.WithID("parent"), session.WithRootID("parent"))

	_, err = rt.managedSubagents.handleStart(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"worker","task":"wait"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	<-provider.started
	first := <-provider.seen
	require.NotEmpty(t, first)
	require.NotContains(t, messagesContent(first), "child followup")
	require.NoError(t, rt.FollowUp(QueuedMessage{Content: "parent followup"}))

	var id string
	rt.managedSubagents.mu.RLock()
	for key := range rt.managedSubagents.items {
		id = key
	}
	rt.managedSubagents.mu.RUnlock()
	result, err := rt.managedSubagents.handleSend(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q,"message":"child followup"}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	close(provider.release)

	var second []chat.Message
	require.Eventually(t, func() bool {
		select {
		case second = <-provider.seen:
			return containsMessage(second, "child followup")
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	require.NotContains(t, messagesContent(second), "parent followup")
}

func messagesContent(messages []chat.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		b.WriteString(msg.Content)
	}
	return b.String()
}

func containsMessage(messages []chat.Message, content string) bool {
	return strings.Contains(messagesContent(messages), content)
}

func TestManagedSubagentFinalizeQueuesCleanRequest(t *testing.T) {
	provider := &blockingTestProvider{id: "test/worker", started: make(chan struct{}), release: make(chan struct{}), content: "done"}
	rt, parent := newSubagentTestRuntime(t, provider)
	_, err := rt.managedSubagents.handleStart(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"worker","task":"wait"}`}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	<-provider.started
	var id string
	rt.managedSubagents.mu.RLock()
	for key := range rt.managedSubagents.items {
		id = key
	}
	rt.managedSubagents.mu.RUnlock()

	result, err := rt.managedSubagents.handleFinalize(t.Context(), parent, tools.ToolCall{Function: tools.FunctionCall{Arguments: fmt.Sprintf(`{"subagent_id":%q}`, id)}}, EventSinkFunc(func(Event) {}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, managedSubagentStatusFinalizing, rt.managedSubagents.items[id].loadStatus())
	msg, ok := rt.managedSubagents.items[id].queue.followUp.Dequeue(t.Context())
	require.True(t, ok)
	require.Contains(t, msg.Content, "finalize cleanly")
	close(provider.release)
}
