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
	return &runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{ID: "child", AgentName: "greppy", Live: true}}, nil
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
	attached := NewAttached(ctx, rt, stored, runtime.LiveSessionNode{ID: "child", AgentName: "greppy"})

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
