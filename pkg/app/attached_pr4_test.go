package app

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type attachedLiveRuntime struct {
	*mockRuntime

	followID string
	snapshot []runtime.Event
	stream   chan runtime.Event
}

func (r *attachedLiveRuntime) AttachLiveSession(context.Context, string) (<-chan runtime.Event, func(), error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, func() {}, nil
}

func (r *attachedLiveRuntime) AttachLiveSessionWithSnapshot(context.Context, string, int) ([]runtime.Event, <-chan runtime.Event, error) {
	if r.stream == nil {
		r.stream = make(chan runtime.Event)
		close(r.stream)
	}
	return r.snapshot, r.stream, nil
}

func (r *attachedLiveRuntime) LiveSessionTree(context.Context, string) (*runtime.LiveSessionTree, error) {
	return &runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{ID: "child", AgentName: "greppy", Live: true, Status: "waiting"}}, nil
}

func (r *attachedLiveRuntime) LiveChildSession(string) (*session.Session, bool) {
	return nil, false
}

func (r *attachedLiveRuntime) SteerSessionByID(string, runtime.QueuedMessage) error {
	return nil
}

func (r *attachedLiveRuntime) FollowUpSessionByID(id string, msg runtime.QueuedMessage) error {
	r.followID = id
	return nil
}
func (r *attachedLiveRuntime) InterruptSessionByID(string) error { return nil }
func (r *attachedLiveRuntime) CloseSessionByID(string) error     { return nil }
func (r *attachedLiveRuntime) StopSessionByID(string) error      { return nil }

type attachedStartupRuntime struct {
	*mockRuntime

	emittedAgent string
}

func (r *attachedStartupRuntime) EmitSessionStartupInfo(_ context.Context, _ *session.Session, agentName string, events runtime.EventSink) {
	r.emittedAgent = agentName
	events.Emit(runtime.TeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-root"},
		{Name: "greppy", Description: "Scout", Provider: "anthropic", Model: "claude"},
	}, agentName))
	events.Emit(runtime.AgentInfo(agentName, "anthropic/claude", "Scout", ""))
	events.Emit(runtime.ToolsetInfo(7, false, agentName))
}

func TestNewAttachedHydratesSidebarStartupInfoForAttachedAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rt := &attachedStartupRuntime{mockRuntime: &mockRuntime{}}
	sess := session.New(session.WithID("child"), session.WithAgentName("greppy"))
	attached := NewAttached(ctx, rt, sess, runtime.LiveSessionNode{ID: "child", AgentName: "greppy", Live: false, Status: "waiting"})

	received := make(chan runtime.Event, 8)
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	go attached.SubscribeWith(subCtx, func(msg tea.Msg) {
		if ev, ok := msg.(runtime.Event); ok {
			received <- ev
		}
	})

	var sawTeam, sawAgent, sawTools bool
	require.Eventually(t, func() bool {
		for {
			select {
			case ev := <-received:
				switch ev := ev.(type) {
				case *runtime.TeamInfoEvent:
					sawTeam = ev.CurrentAgent == "greppy" && len(ev.AvailableAgents) == 2
				case *runtime.AgentInfoEvent:
					sawAgent = ev.AgentName == "greppy" && ev.Model == "anthropic/claude"
				case *runtime.ToolsetInfoEvent:
					sawTools = ev.GetAgentName() == "greppy" && ev.AvailableTools == 7 && !ev.Loading
				}
			default:
				return sawTeam && sawAgent && sawTools
			}
		}
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "greppy", rt.emittedAgent)
}

func TestNewAttachedUsesSessionIDForSendAndLiveTail(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stored := session.New(session.WithID("child"))
	stored.AddMessage(session.UserMessage("stored prompt"))
	liveTail := make(chan runtime.Event, 1)
	rt := &attachedLiveRuntime{
		mockRuntime: &mockRuntime{},
		stream:      liveTail,
	}
	attached := NewAttached(ctx, rt, stored, runtime.LiveSessionNode{ID: "child", AgentName: "greppy", Live: true, Status: "waiting"})

	received := make(chan runtime.Event, 8)
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	go attached.SubscribeWith(subCtx, func(msg tea.Msg) {
		if ev, ok := msg.(runtime.Event); ok {
			received <- ev
		}
	})

	liveTail <- runtime.AgentChoice("greppy", "child", "live tail")
	require.Eventually(t, func() bool {
		for {
			select {
			case ev := <-received:
				if choice, ok := ev.(*runtime.AgentChoiceEvent); ok && choice.Content == "live tail" {
					return true
				}
			default:
				return false
			}
		}
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, attached.FollowUpWithAttachments("hello child", nil))
	require.Equal(t, "child", rt.followID)
}
