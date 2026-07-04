package app

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type replyStream struct{ idx int }

func (s *replyStream) Recv() (chat.MessageStreamResponse, error) {
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

func (s *replyStream) Close() {}

type replyProvider struct{ stubProvider }

func (replyProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &replyStream{}, nil
}

// Every tab is a viewer of its session's event stream: a runtime-owned wake
// run (the session actor answering a subagent note while the tab is idle)
// renders on the App bus exactly like a turn the user started. This is the
// consolidation that removed the receiver-managed/actor-managed split.
func TestWakeRunRendersOnAppBus(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(replyProvider{}))))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)

	sess := session.New(session.WithID("viewer-sess"))
	a := New(t.Context(), rt, sess)
	a.Start(t.Context())

	events := make(chan tea.Msg, 256)
	go a.SubscribeWith(t.Context(), func(msg tea.Msg) { events <- msg })
	waitForStartup(t, events)

	// One user-driven turn first (proves the bridge carries the App's own
	// runs — the classic path).
	runCtx, cancel := context.WithCancel(t.Context())
	a.Run(runCtx, cancel, "hello", nil)
	waitForStop(t, events, "the App's own turn must render on the bus")

	// A run this App did not start (stand-in for the session actor waking
	// the session on a subagent note — the wake mechanics themselves are
	// covered in pkg/runtime): the tab renders it live, no receiver wiring
	// anywhere. RunOrAttachStream is the actor door, so this cannot race the
	// App's own run winding down.
	sess.AddMessage(session.UserMessage("<system_info>subagent report</system_info>"))
	go func() {
		for range runtime.RunOrAttachStream(context.WithoutCancel(t.Context()), rt, sess) {
		}
	}()
	waitForStop(t, events, "a runtime-owned run must render on the bus")
}

func waitForStop(t *testing.T, events <-chan tea.Msg, msg string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-events:
			if _, ok := e.(*runtime.StreamStoppedEvent); ok {
				return
			}
		case <-deadline:
			t.Fatal(msg)
		}
	}
}

// waitForStartup drains events until the async startup-info emit finishes
// (it reads the session's usage, which a run writes — running earlier races
// EmitStartupInfo; same wait as the attach tests).
func waitForStartup(t *testing.T, events <-chan tea.Msg) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-events:
			if e, ok := msg.(*runtime.ToolsetInfoEvent); ok && !e.Loading {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for startup info")
		}
	}
}

// Retry and RunWithMessage must not feed the bus themselves when the bridge
// is active — the run's events arrive through the hub, and a second direct
// forward doubles every delta (the "mixed messages" regression).
func TestBridgedRetryDoesNotDuplicateEvents(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(replyProvider{}))))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)

	sess := session.New(session.WithID("retry-sess"))
	sess.AddMessage(session.UserMessage("hello"))
	a := New(t.Context(), rt, sess)
	a.Start(t.Context())

	events := make(chan tea.Msg, 256)
	go a.SubscribeWith(t.Context(), func(msg tea.Msg) { events <- msg })
	waitForStartup(t, events)

	a.Retry(t.Context(), func() {})

	var content strings.Builder
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-events:
			switch e := msg.(type) {
			case *runtime.AgentChoiceEvent:
				content.WriteString(e.Content)
			case *runtime.StreamStoppedEvent:
				assert.Equal(t, "ok", content.String(), "each delta must reach the bus exactly once")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the retried turn")
		}
	}
}

// The cancel gate mutes exactly the cancelled run's tail: the run's
// stream-stop releases it, so a later run (a wake run, a retry) renders
// normally instead of vanishing behind a stale gate.
func TestCancelGateReleasesOnStreamStop(t *testing.T) {
	t.Parallel()

	a := &App{}
	a.MarkRunCancelled()

	assert.Nil(t, a.filterBridgedEvent(&runtime.AgentChoiceEvent{}), "tail events of the cancelled run are muted")
	assert.NotNil(t, a.filterBridgedEvent(&runtime.StreamStoppedEvent{}), "the stop must pass to clear spinners")
	assert.NotNil(t, a.filterBridgedEvent(&runtime.AgentChoiceEvent{}), "the next run's events must render")
}

// The bridge must be lossless even when the bus is slow: the elastic pump
// keeps draining the hub subscription, so the hub's drop-on-full protection
// never discards events the tab is going to render.
func TestPumpElasticNeverDropsUnderBackpressure(t *testing.T) {
	t.Parallel()

	in := make(chan int, 4)
	out := pumpElastic(t.Context(), in)

	// Send everything before anyone reads out: if the pump ever stopped
	// draining, these sends would block and the test would time out.
	const n = 10_000
	for i := range n {
		in <- i
	}
	close(in)

	var got int
	for range out {
		got++
	}
	assert.Equal(t, n, got)
}
