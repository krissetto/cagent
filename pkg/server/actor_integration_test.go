package server

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// scriptedStream replays a fixed reply then EOF; scriptedProvider hands a
// fresh one out per model call so a session can run several turns.
type scriptedStream struct{ idx int }

func (s *scriptedStream) Recv() (chat.MessageStreamResponse, error) {
	defer func() { s.idx++ }()
	switch s.idx {
	case 0:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{
			Delta: chat.MessageDelta{Content: "ok"},
		}}}, nil
	case 1:
		return chat.MessageStreamResponse{
			Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}},
			Usage:   &chat.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}

func (s *scriptedStream) Close() {}

type scriptedProvider struct{}

func (p *scriptedProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/mock-model") }
func (p *scriptedProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &scriptedStream{}, nil
}
func (p *scriptedProvider) BaseConfig() base.Config { return base.Config{} }
func (p *scriptedProvider) MaxTokens() int          { return 0 }

// The hub-fed event log is what makes runtime-initiated runs (async subagent
// wake-ups between requests) visible to clients tailing
// GET /api/sessions/:id/events: every run of the session lands in the log,
// whoever started it, and RunSession does not double-append its own events.
func TestHubEventLogCarriesRuntimeInitiatedRuns(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithID("hub-log-sess"))
	require.NoError(t, store.AddSession(ctx, sess))

	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&scriptedProvider{}))))
	rt, err := runtime.New(ctx, tm, runtime.WithSessionStore(store))
	require.NoError(t, err)

	sm := &SessionManager{
		runtimeSessions:   concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions:   concurrent.NewMap[string, *activeRuntimes](),
		eventLogs:         concurrent.NewMap[string, *pumpedEventLog](),
		followUpInjectors: concurrent.NewMap[string, FollowUpInjector](),
		followUpKeys:      concurrent.NewMap[string, *idempotencyCache](),
		sessionStore:      store,
		Sources:           config.Sources{},
		runConfig:         &config.RuntimeConfig{},
		sessionReady:      make(chan struct{}),
	}
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{
		runtime:  rt,
		session:  sess,
		titleGen: (*sessiontitle.Generator)(nil),
	})

	ar, ok := rt.(actorRuntime)
	require.True(t, ok, "local runtimes must expose the actor capability")
	sm.registerHubEventLog(ctx, sess.ID, ar)
	require.True(t, sm.HasEventSource(sess.ID))

	// A run the server did not start (stand-in for an actor wake run).
	sess.AddMessage(session.UserMessage("hi"))
	for range rt.RunStream(ctx, sess) {
	}

	countStops := func() int {
		stops := 0
		streamCtx, cancelStream := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancelStream()
		sm.StreamEvents(streamCtx, sess.ID, nil, func(_ uint64, event any) {
			if _, ok := event.(*runtime.StreamStoppedEvent); ok {
				stops++
			}
		})
		return stops
	}
	assert.Eventually(t, func() bool { return countStops() == 1 },
		5*time.Second, 50*time.Millisecond, "the runtime-initiated run must reach the event log")

	// A client turn through RunSession: streamed to the caller AND logged
	// exactly once (no manual append on top of the hub feed).
	events, err := sm.RunSession(ctx, sess.ID, "", "", []api.Message{{Content: "again"}}, "")
	require.NoError(t, err)
	for range events {
	}
	assert.Eventually(t, func() bool { return countStops() == 2 },
		5*time.Second, 50*time.Millisecond, "the client run must be logged exactly once")
}
