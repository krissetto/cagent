package runtime

import (
	"context"
	"fmt"
	"strings"
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
	"github.com/docker/docker-agent/pkg/tools/builtin/subagents"
)

func TestNestedSubagentTurnsPropagateEnvelopesToRootAndRootStopsWhenIdle(t *testing.T) {
	gate := make(chan struct{})

	rootModel := &scriptedNestedProvider{id: "test/root", streams: []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("root-start-director", ToolNameSubagentStart).
			AddToolCallArguments("root-start-director", `{"agent":"director","task":"coordinate nested work"}`).
			AddToolCallStopWithUsage(1, 1).
			Build(),
		newStreamBuilder().AddContent("root is waiting on director").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root saw director waiting").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root saw director final").AddStopWithUsage(1, 1).Build(),
	}}
	directorModel := &scriptedNestedProvider{id: "test/director", streams: []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("director-start-implementer", ToolNameSubagentStart).
			AddToolCallArguments("director-start-implementer", `{"agent":"implementer","task":"do nested edit"}`).
			AddToolCallStopWithUsage(1, 1).
			Build(),
		newStreamBuilder().AddContent("waiting on implementer").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("director saw implementer").AddStopWithUsage(1, 1).Build(),
	}}
	implementerModel := &scriptedNestedProvider{id: "test/implementer", wait: []<-chan struct{}{gate}, streams: []chat.MessageStream{
		newStreamBuilder().AddContent("implementer done").AddStopWithUsage(1, 1).Build(),
	}}

	subagentTools := subagents.New(nil)
	implementer := agent.New("implementer", "implementer", agent.WithModel(implementerModel), agent.WithTitleModel(&staticTitleProvider{title: "Implementer title"}))
	director := agent.New("director", "director",
		agent.WithModel(directorModel),
		agent.WithTitleModel(&staticTitleProvider{title: "Director title"}),
		agent.WithToolSets(subagentTools),
		agent.WithSubAgents(implementer),
		agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "implementer", Agent: "implementer"}),
	)
	root := agent.New("root", "root",
		agent.WithModel(rootModel),
		agent.WithToolSets(subagentTools),
		agent.WithSubAgents(director),
		agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "director", Agent: "director"}),
	)

	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, director, implementer)), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)

	rootSession := session.New(session.WithID("root-session"), session.WithAgentName("root"), session.WithUserMessage("start"), session.WithToolsApproved(true))
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), rootSession))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var events []Event
	var released bool
	for ev := range rt.RunStream(ctx, rootSession) {
		events = append(events, ev)
		if envelope, ok := ev.(*UserMessageEvent); ok && envelope.Kind == session.MessageKindSubagentEnvelope && strings.Contains(envelope.Message, "waiting on implementer") && !released {
			released = true
			close(gate)
		}
	}

	require.NoError(t, ctx.Err(), "root stream should stop once direct child and grandchild are idle")
	require.True(t, released, "root never received the director's waiting-turn envelope")
	assertSubagentEnvelopeEvent(t, events, "waiting on implementer")
	assertSubagentEnvelopeEvent(t, events, "director saw implementer")
	assert.True(t, transcriptContainsSubagentEnvelope(rootSession, "waiting on implementer"))
	assert.True(t, transcriptContainsSubagentEnvelope(rootSession, "director saw implementer"))

	var sawStopped bool
	for _, ev := range events {
		if stopped, ok := ev.(*StreamStoppedEvent); ok && stopped.SessionID == rootSession.ID {
			sawStopped = true
		}
	}
	assert.True(t, sawStopped, "root stream must emit StreamStopped after nested subagents are all idle")
}

func assertSubagentEnvelopeEvent(t *testing.T, events []Event, text string) {
	t.Helper()
	for _, ev := range events {
		msg, ok := ev.(*UserMessageEvent)
		if !ok || msg.Kind != session.MessageKindSubagentEnvelope {
			continue
		}
		if strings.Contains(msg.Message, text) {
			return
		}
	}
	t.Fatalf("missing subagent envelope containing %q; events: %#v", text, events)
}

func transcriptContainsSubagentEnvelope(sess *session.Session, text string) bool {
	if sess == nil {
		return false
	}
	for _, item := range sess.Messages {
		if item.Message == nil || item.Message.Kind != session.MessageKindSubagentEnvelope {
			continue
		}
		if strings.Contains(item.Message.Message.Content, text) {
			return true
		}
	}
	return false
}

type scriptedNestedProvider struct {
	id      string
	wait    []<-chan struct{}
	streams []chat.MessageStream

	mu    sync.Mutex
	calls int
}

func (p *scriptedNestedProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }

func (p *scriptedNestedProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	var wait <-chan struct{}
	if idx < len(p.wait) {
		wait = p.wait[idx]
	}
	var stream chat.MessageStream
	if idx < len(p.streams) {
		stream = p.streams[idx]
	}
	p.mu.Unlock()

	if wait != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
	if stream == nil {
		return nil, fmt.Errorf("unexpected model call %d for %s", idx+1, p.id)
	}
	return stream, nil
}

func (p *scriptedNestedProvider) BaseConfig() base.Config { return base.Config{} }

func (p *scriptedNestedProvider) MaxTokens() int { return 0 }
