package runtime

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// multiRunProvider hands out a fresh scripted stream per model call, so a
// session can run multiple turns (initial run + actor wakes).
type multiRunProvider struct {
	*mockProvider

	build func() chat.MessageStream
}

func (p *multiRunProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.build(), nil
}

type blockingMockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
	release   <-chan struct{}
}

func (s *blockingMockStream) Recv() (chat.MessageStreamResponse, error) {
	if s.idx == len(s.responses) {
		s.idx++
		<-s.release
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}
	if s.idx > len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := s.responses[s.idx]
	s.idx++
	return resp, nil
}

func (s *blockingMockStream) Close() {}

func newActorFixture(t *testing.T) (*LocalRuntime, *session.Session) {
	t.Helper()
	prov := &multiRunProvider{
		mockProvider: &mockProvider{id: "test/mock-model"},
		build: func() chat.MessageStream {
			return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
		},
	}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return rt, session.New(session.WithID("actor-sess"))
}

func TestActorWakesIdleSessionOnNote(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)

	sess.AddMessage(session.UserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}
	turnOne := len(sess.Messages)

	_, events, cancel := rt.SubscribeSessionEvents(sess.ID)
	defer cancel()

	rt.deliverOrBuffer(t.Context(), sess.ID, "<system_info>subagent report</system_info>")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if _, ok := e.(*StreamStoppedEvent); ok {
				assert.Greater(t, len(sess.Messages), turnOne,
					"the wake run must add the note and a fresh assistant reply")
				last := sess.Messages[len(sess.Messages)-1]
				require.NotNil(t, last.Message)
				assert.Equal(t, chat.MessageRoleAssistant, last.Message.Message.Role)
				assert.Eventually(t, func() bool { return rt.sessionSettled(sess.ID) },
					2*time.Second, 10*time.Millisecond)
				return
			}
		case <-deadline:
			t.Fatal("no wake run happened for the buffered note")
		}
	}
}

func TestActorBuffersUnknownSessionUntilSeen(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)

	rt.deliverOrBuffer(t.Context(), sess.ID, "early note")
	assert.False(t, rt.sessionSettled(sess.ID), "note must stay buffered")

	sess.AddMessage(session.UserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	assert.Eventually(t, func() bool {
		return rt.sessionSettled(sess.ID) && len(sess.Messages) >= 3
	}, 5*time.Second, 10*time.Millisecond)
	assert.Contains(t, sess.Messages[1].Message.Message.Content, "early note")
}

func TestRunOrAttachDrivesFreeSession(t *testing.T) {
	t.Parallel()

	rt, sess := newActorFixture(t)

	sess.AddMessage(session.UserMessage("hello"))
	var sawStop bool
	for e := range rt.RunOrAttach(t.Context(), sess) {
		if _, ok := e.(*StreamStoppedEvent); ok {
			sawStop = true
		}
	}
	assert.True(t, sawStop)
	require.GreaterOrEqual(t, len(sess.Messages), 2)
	require.NotNil(t, sess.Messages[0].Message)
	assert.Equal(t, "hello", sess.Messages[0].Message.Message.Content)
	assert.Equal(t, chat.MessageRoleUser, sess.Messages[0].Message.Message.Role)
	assert.Eventually(t, func() bool { return rt.sessionSettled(sess.ID) },
		2*time.Second, 10*time.Millisecond)
}

func TestRunOrAttachMirrorsLiveRunThenDrives(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	first := true
	prov := &multiRunProvider{
		mockProvider: &mockProvider{id: "test/mock-model"},
		build: func() chat.MessageStream {
			if first {
				first = false
				return &blockingMockStream{
					responses: []chat.MessageStreamResponse{{
						Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: "report handled"}}},
					}},
					release: release,
				}
			}
			return newStreamBuilder().AddContent("queued answered").AddStopWithUsage(1, 1).Build()
		},
	}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	sess := session.New(session.WithID("attach-sess"))
	rt.rememberSession(sess)

	rt.deliverOrBuffer(t.Context(), sess.ID, "wake note")
	require.Eventually(t, func() bool {
		d, ok := rt.sessionDrivers.Lookup(sess.ID)
		return ok && !d.Settled()
	}, 2*time.Second, 5*time.Millisecond)

	sess.AddMessage(session.UserMessage("queued"))
	ch := rt.RunOrAttach(t.Context(), sess)
	require.Eventually(t, func() bool {
		rt.sessionEvents.mu.Lock()
		defer rt.sessionEvents.mu.Unlock()
		return len(rt.sessionEvents.subs[sess.ID]) == 1
	}, 2*time.Second, 5*time.Millisecond)

	close(release)

	var mirrored bool
	var stops int
	for e := range ch {
		switch ev := e.(type) {
		case *AgentChoiceEvent:
			if ev.Content == "report handled" {
				mirrored = true
			}
		case *StreamStoppedEvent:
			stops++
		}
	}
	assert.True(t, mirrored, "the live run's tail is mirrored to the caller")
	assert.Equal(t, 2, stops, "mirrored stop + the staged turn's own stop")
	last := sess.Messages[len(sess.Messages)-1]
	require.NotNil(t, last.Message)
	assert.Equal(t, chat.MessageRoleAssistant, last.Message.Message.Role)
}

func TestStopSessionCancelsWakeRun(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	prov := &activeRootBlockingProvider{id: "test/blocking-model", release: release}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	t.Cleanup(func() { close(release) })

	sess := session.New(session.WithID("stop-sess"))
	rt.rememberSession(sess)

	rt.deliverOrBuffer(t.Context(), sess.ID, "note")
	require.Eventually(t, func() bool {
		d, ok := rt.sessionDrivers.Lookup(sess.ID)
		return ok && !d.Settled()
	}, 2*time.Second, 5*time.Millisecond, "the note must start a wake run")

	require.True(t, rt.StopSession(sess.ID), "a live wake run must be stoppable")
	assert.Eventually(t, func() bool { return rt.sessionSettled(sess.ID) },
		5*time.Second, 10*time.Millisecond, "the wake run must wind down after StopSession")

	assert.False(t, rt.StopSession(sess.ID), "nothing left to stop")
}
