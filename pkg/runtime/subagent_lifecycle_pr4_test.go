package runtime

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSubagentSessionTitleGeneratedForChildAndPublished(t *testing.T) {
	rt, root, _ := newSubagentLifecycleRuntime(t, 2*time.Second)

	h, err := rt.subagents.Start(t.Context(), root, "worker", "first prompt")
	require.NoError(t, err)
	requireSubagentState(t, h, "waiting")
	requireSubagentTitle(t, rt, h.id, "Child title")

	snapshot := rt.eventBus.StreamingSnapshot(h.id)
	assertEventHasTitle(t, snapshot, h.id, "Child title")

	stored, err := rt.sessionStore.GetSession(t.Context(), h.id)
	require.NoError(t, err)
	assert.Equal(t, "Child title", stored.Title)

	tree, err := rt.LiveSessionTree(t.Context(), root.ID)
	require.NoError(t, err)
	require.NotNil(t, tree.Root)
	require.Len(t, tree.Root.Children, 1)
	assert.Equal(t, h.id, tree.Root.Children[0].ID)
	assert.Equal(t, "Child title", tree.Root.Children[0].Title)

	require.NoError(t, rt.subagents.Finalize(root, h.id))
	requireSubagentDoneClosed(t, h)
}

func TestSubagentLifecycleRemainsWaitingLiveAfterFirstTurn(t *testing.T) {
	rt, root, childModel := newSubagentLifecycleRuntime(t, 2*time.Second)

	h, err := rt.subagents.Start(t.Context(), root, "worker", "first prompt")
	require.NoError(t, err)
	requireSubagentState(t, h, "waiting")

	assertSubagentDoneOpen(t, h)
	assert.True(t, h.live(), "handle should remain live while waiting")
	assert.Equal(t, []string{"first prompt"}, childModel.prompts())

	listed := rt.subagents.List(root)
	require.Len(t, listed, 1)
	assert.Equal(t, h.id, listed[0].ID)
	assert.Equal(t, "waiting", listed[0].State)

	resolved, err := rt.subagents.ResolveSession(h.shortID)
	require.NoError(t, err)
	assert.Equal(t, h.id, resolved.id)

	live, ok := rt.LiveChildSession(h.id)
	require.True(t, ok)
	assert.Equal(t, h.id, live.ID)

	assert.NotEmpty(t, rt.eventBus.StreamingSnapshot(h.id), "event topic should retain first-turn history while child waits")

	snapshot, events, err := rt.AttachLiveSessionWithSnapshot(t.Context(), h.id, 16)
	require.NoError(t, err)
	assert.NotNil(t, events)
	assert.NotEmpty(t, snapshot, "attach after first turn should hydrate existing child history/events")
	assertSnapshotHasMessage(t, snapshot, h.id, "child reply 1")

	assertSubagentDoneOpen(t, h)
	require.NoError(t, rt.subagents.Stop(root, h.id))
	requireSubagentDoneClosed(t, h)
}

func TestSubagentSendAfterFirstTurnDispatchesAnotherTurn(t *testing.T) {
	rt, root, childModel := newSubagentLifecycleRuntime(t, 2*time.Second)

	h, err := rt.subagents.Start(t.Context(), root, "worker", "first prompt")
	require.NoError(t, err)
	requireSubagentState(t, h, "waiting")
	assertSubagentDoneOpen(t, h)

	require.NoError(t, rt.subagents.Send(root, h.shortID, "second prompt"))
	requireSubagentState(t, h, "waiting")
	assertSubagentDoneOpen(t, h)
	assert.Equal(t, []string{"first prompt", "second prompt"}, childModel.prompts())

	assert.NotEmpty(t, rt.eventBus.StreamingSnapshot(h.id), "event topic should retain child history while child waits")

	snapshot, _, err := rt.AttachLiveSessionWithSnapshot(t.Context(), h.id, 32)
	require.NoError(t, err)
	assertSnapshotHasMessage(t, snapshot, h.id, "child reply 2")

	require.NoError(t, rt.subagents.Finalize(root, h.id))
	requireSubagentDoneClosed(t, h)
}

func TestSubagentDoneClosesOnlyOnExplicitTerminal(t *testing.T) {
	rt, root, _ := newSubagentLifecycleRuntime(t, time.Hour)
	h, err := rt.subagents.Start(t.Context(), root, "worker", "first prompt")
	require.NoError(t, err)
	requireSubagentState(t, h, "waiting")
	assertSubagentDoneOpen(t, h)

	require.NoError(t, rt.subagents.Finalize(root, h.id))
	requireSubagentDoneClosed(t, h)
}

func TestSubagentDoneClosesOnTTL(t *testing.T) {
	rt, root, _ := newSubagentLifecycleRuntime(t, 25*time.Millisecond)
	h, err := rt.subagents.Start(t.Context(), root, "worker", "first prompt")
	require.NoError(t, err)
	requireSubagentState(t, h, "waiting")
	assertSubagentDoneOpen(t, h)

	requireSubagentDoneClosed(t, h)
	requireSubagentState(t, h, "closed")
}

func newSubagentLifecycleRuntime(t *testing.T, ttl time.Duration) (*LocalRuntime, *session.Session, *recordingSubagentProvider) {
	t.Helper()
	childModel := &recordingSubagentProvider{}
	titleModel := &staticTitleProvider{title: "Child title"}
	worker := agent.New("worker", "worker", agent.WithModel(childModel), agent.WithTitleModel(titleModel), agent.WithIdleAutoFinalizeTimeout(ttl))
	rootAgent := agent.New(
		"root",
		"root",
		agent.WithModel(&recordingSubagentProvider{}),
		agent.WithSubAgents(worker),
		agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "worker", Agent: "worker"}),
	)
	rt, err := NewLocalRuntime(team.New(team.WithAgents(rootAgent, worker)), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)
	root := session.New(session.WithID("root-lifecycle"), session.WithAgentName("root"), session.WithToolsApproved(true), session.WithTitle("root"))
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), root))
	if rt.liveSessions != nil {
		rt.liveSessions.register(root.ID, "root", "")
	}
	return rt, root, childModel
}

func requireSubagentState(t *testing.T, h *subagentHandle, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.state == want
	}, time.Second, 5*time.Millisecond)
}

func requireSubagentTitle(t *testing.T, rt *LocalRuntime, sessionID, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		stored, err := rt.sessionStore.GetSession(t.Context(), sessionID)
		return err == nil && stored.Title == want
	}, time.Second, 5*time.Millisecond)
}

func assertSubagentDoneOpen(t *testing.T, h *subagentHandle) {
	t.Helper()
	select {
	case <-h.done:
		t.Fatalf("subagent done closed unexpectedly")
	default:
	}
}

func requireSubagentDoneClosed(t *testing.T, h *subagentHandle) {
	t.Helper()
	require.Eventually(t, func() bool {
		select {
		case <-h.done:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
}

func assertSnapshotHasMessage(t *testing.T, snapshot []Event, sessionID, content string) {
	t.Helper()
	for _, ev := range snapshot {
		msg, ok := ev.(*MessageAddedEvent)
		if !ok || msg.SessionID != sessionID || msg.Message == nil {
			continue
		}
		if msg.Message.Message.Content == content {
			return
		}
	}
	t.Fatalf("snapshot missing message %q for session %s; got %#v", content, sessionID, snapshot)
}

func assertEventHasTitle(t *testing.T, events []Event, sessionID, title string) {
	t.Helper()
	for _, ev := range events {
		titleEv, ok := ev.(*SessionTitleEvent)
		if ok && titleEv.SessionID == sessionID && titleEv.Title == title {
			return
		}
	}
	t.Fatalf("events missing title %q for session %s; got %#v", title, sessionID, events)
}

type recordingSubagentProvider struct {
	mu      sync.Mutex
	inputs  []string
	streams int
}

func (p *recordingSubagentProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/subagent-lifecycle")
}

func (p *recordingSubagentProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streams++
	p.inputs = append(p.inputs, lastUserContent(messages))
	responses := lifecycleResponses(fmt.Sprintf("child reply %d", p.streams))
	return &recordingLifecycleStream{responses: responses}, nil
}

func (p *recordingSubagentProvider) BaseConfig() base.Config { return base.Config{} }

func (p *recordingSubagentProvider) MaxTokens() int { return 0 }

type staticTitleProvider struct{ title string }

func (p *staticTitleProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/title") }

func (p *staticTitleProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &recordingLifecycleStream{responses: lifecycleResponses(p.title)}, nil
}

func (p *staticTitleProvider) BaseConfig() base.Config { return base.Config{} }

func (p *staticTitleProvider) MaxTokens() int { return 0 }

func (p *recordingSubagentProvider) prompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.inputs...)
}

func lifecycleResponses(content string) []chat.MessageStreamResponse {
	return []chat.MessageStreamResponse{
		{
			Choices: []chat.MessageStreamChoice{{
				Index: 0,
				Delta: chat.MessageDelta{Content: content},
			}},
		},
		{
			Choices: []chat.MessageStreamChoice{{
				Index:        0,
				Delta:        chat.MessageDelta{},
				FinishReason: chat.FinishReasonStop,
			}},
			Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1},
		},
	}
}

type recordingLifecycleStream struct {
	responses []chat.MessageStreamResponse
	idx       int
}

func (s *recordingLifecycleStream) Recv() (chat.MessageStreamResponse, error) {
	if s.idx >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := s.responses[s.idx]
	s.idx++
	return resp, nil
}

func (s *recordingLifecycleStream) Close() {}

func lastUserContent(messages []chat.Message) string {
	for i := len(messages) - 1; i >= 0; i-- { //nolint:modernize // Need compatibility with current Go toolchain range forms.
		if messages[i].Role == chat.MessageRoleUser {
			return messages[i].Content
		}
	}
	return ""
}
