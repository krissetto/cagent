package app

import (
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// An attached subagent tab's startup info must describe the child's pinned
// agent everywhere. The TUI derives the "selected agent" from event agent
// names — including the generic AgentContext fallback — so ANY startup event
// stamped with the runtime's global current agent (the root) would overwrite
// the selection back to root. This pins the full startup stream, on a runtime
// whose once-flag was already consumed by the parent's App.
func TestAttachedAppStartupEventsDescribeChildAgent(t *testing.T) {
	t.Parallel()

	store, err := session.NewSQLiteSessionStore(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer store.(*session.SQLiteSessionStore).Close()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(stubProvider{})),
		agent.New("planner", "prompt", agent.WithModel(stubProvider{})),
	))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionStore(store))
	require.NoError(t, err)

	// The parent's App consumes the runtime's startup info first (once-flag).
	// Wait for its startup stream to finish — as in reality, where the parent
	// tab started long before the user attaches — so the attached App's
	// ResetStartupInfo cannot race the parent's in-flight emit.
	parentApp := New(t.Context(), rt, session.New(session.WithID("parent")))
	parentApp.Start(t.Context())
	parentEvents := make(chan tea.Msg, 256)
	go parentApp.SubscribeWith(t.Context(), func(msg tea.Msg) { parentEvents <- msg })
	parentDeadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case msg := <-parentEvents:
			if e, ok := msg.(*runtime.ToolsetInfoEvent); ok && !e.Loading {
				done = true
			}
		case <-parentDeadline:
			t.Fatal("timed out waiting for the parent app's startup info")
		}
	}

	childSess := session.New(session.WithID("child"))
	childSess.AgentName = "planner"
	info := runtime.SubagentAttachInfo{NodeID: "77c88", Agent: "planner", Session: childSess, ParentSessionID: "parent", ParentAgent: "root"}
	a := New(t.Context(), rt, childSess, WithSubagentAttach(info))
	a.Start(t.Context())

	events := make(chan tea.Msg, 256)
	go a.SubscribeWith(t.Context(), func(msg tea.Msg) { events <- msg })

	// Drain until the final ToolsetInfo (loading finished); every
	// agent-stamped event along the way must name the child agent.
	deadline := time.After(10 * time.Second)
	var sawAgentInfo, sawTeamInfo, sawFinalTools bool
	for !sawAgentInfo || !sawTeamInfo || !sawFinalTools {
		select {
		case msg := <-events:
			switch e := msg.(type) {
			case *runtime.AgentInfoEvent:
				require.Equal(t, "planner", e.AgentName)
				sawAgentInfo = true
			case *runtime.TeamInfoEvent:
				require.Equal(t, "planner", e.CurrentAgent)
				sawTeamInfo = true
			case *runtime.ToolsetInfoEvent:
				require.Equal(t, "planner", e.GetAgentName(), "toolset info must describe the pinned agent, not the runtime's current agent")
				if !e.Loading {
					sawFinalTools = true
				}
			default:
				if ev, ok := msg.(runtime.Event); ok {
					if name := ev.GetAgentName(); name != "" {
						require.Equal(t, "planner", name, "startup event %T stamped with the wrong agent", msg)
					}
				}
			}
		case <-deadline:
			t.Fatalf("timed out (agentInfo=%v teamInfo=%v finalTools=%v)", sawAgentInfo, sawTeamInfo, sawFinalTools)
		}
	}
}
