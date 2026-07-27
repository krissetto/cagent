package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

func TestPlanChangedEventSerialization(t *testing.T) {
	t.Parallel()

	ev := PlanChanged("shared", "release", plan.ChangeActionWrite, 3, "architect")
	data, err := json.Marshal(ev)
	require.NoError(t, err)

	var decoded PlanChangedEvent
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "plan_changed", decoded.Type)
	assert.Equal(t, "shared", decoded.Scope)
	assert.Equal(t, "release", decoded.Name)
	assert.Equal(t, "write", decoded.Action)
	assert.Equal(t, 3, decoded.Version)
	assert.Equal(t, "architect", decoded.GetAgentName())
	assert.NotContains(t, string(data), "content", "the event must never carry plan content")
}

// TestClient_DecodesPlanChangedEvent verifies the event is registered in the
// client's event registry, so remote runtimes deliver it typed rather than
// dropping it.
func TestClient_DecodesPlanChangedEvent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"type\":\"plan_changed\",\"scope\":\"shared\",\"name\":\"release\",\"action\":\"status\",\"version\":4}\n\n")
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL)
	require.NoError(t, err)

	ch, err := c.StreamSessionEvents(t.Context(), "s")
	require.NoError(t, err)

	var got *PlanChangedEvent
	for ev := range ch {
		if planEv, ok := ev.(*PlanChangedEvent); ok {
			got = planEv
			break
		}
	}
	require.NotNil(t, got, "plan_changed must decode to *PlanChangedEvent")
	assert.Equal(t, "release", got.Name)
	assert.Equal(t, "status", got.Action)
	assert.Equal(t, 4, got.Version)
	assert.Equal(t, "shared", got.Scope)
}

// planToolHandler finds one of the plan toolset's tools by name. The write
// goes through the tool handler (not the storage) so the test exercises the
// same code path as an agent's tool call.
func planToolHandler(t *testing.T, ts tools.ToolSet, name string) tools.ToolHandler {
	t.Helper()
	all, err := ts.Tools(t.Context())
	require.NoError(t, err)
	for _, tl := range all {
		if tl.Name == name {
			return tl.Handler
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func planToolCall(t *testing.T, args any) tools.ToolCall {
	t.Helper()
	payload, err := json.Marshal(args)
	require.NoError(t, err)
	call := tools.ToolCall{}
	call.Function.Arguments = string(payload)
	return call
}

// TestConfigureToolsetHandlers_WiresPlanChangeEvents verifies that the plan
// toolset's change callback is wired through the agent's toolset wrappers
// (agent.WithToolSets wraps in a StartableToolSet) and that successful
// mutations emit PlanChangedEvent while failed ones emit nothing.
func TestConfigureToolsetHandlers_WiresPlanChangeEvents(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/plan-model", stream: &mockStream{}}
	planTool := plan.New(plan.WithStorage(plan.NewFilesystemStorage(t.TempDir())))

	root := agent.New("root", "agent",
		agent.WithModel(prov),
		agent.WithToolSets(planTool),
	)
	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	events := make(chan Event, 8)
	rt.configureToolsetHandlers(root, NewChannelSink(events))

	write := planToolHandler(t, planTool, plan.ToolNameWritePlan)
	setStatus := planToolHandler(t, planTool, plan.ToolNameSetPlanStatus)
	del := planToolHandler(t, planTool, plan.ToolNameDeletePlan)

	// Successful write emits a versioned event without content.
	result, err := write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v1"}), nil)
	require.NoError(t, err)
	require.False(t, result.IsError)
	ev := requirePlanChanged(t, events)
	assert.Equal(t, "shared", ev.Scope)
	assert.Equal(t, "release", ev.Name)
	assert.Equal(t, plan.ChangeActionWrite, ev.Action)
	assert.Equal(t, 1, ev.Version)
	assert.Equal(t, "root", ev.GetAgentName())

	// Successful status change.
	result, err = setStatus(t.Context(), planToolCall(t, plan.SetPlanStatusArgs{Name: "release", Status: "done"}), nil)
	require.NoError(t, err)
	require.False(t, result.IsError)
	ev = requirePlanChanged(t, events)
	assert.Equal(t, plan.ChangeActionStatus, ev.Action)
	assert.Equal(t, 2, ev.Version)

	// Failed write (version conflict) emits nothing.
	stale := 42
	result, err = write(t.Context(), planToolCall(t, plan.WritePlanArgs{Name: "release", Content: "v2", LastKnownRevision: &stale}), nil)
	require.NoError(t, err)
	require.True(t, result.IsError)
	requireNoEvent(t, events)

	// Successful guarded delete.
	rev := 2
	result, err = del(t.Context(), planToolCall(t, plan.DeletePlanArgs{Name: "release", LastKnownRevision: &rev}), nil)
	require.NoError(t, err)
	require.False(t, result.IsError)
	ev = requirePlanChanged(t, events)
	assert.Equal(t, plan.ChangeActionDelete, ev.Action)
	assert.Equal(t, 2, ev.Version)
}

func requirePlanChanged(t *testing.T, events chan Event) *PlanChangedEvent {
	t.Helper()
	select {
	case ev := <-events:
		planEv, ok := ev.(*PlanChangedEvent)
		require.True(t, ok, "expected *PlanChangedEvent, got %T", ev)
		return planEv
	default:
		t.Fatal("expected a PlanChangedEvent, channel is empty")
		return nil
	}
}

func requireNoEvent(t *testing.T, events chan Event) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("expected no event, got %T", ev)
	default:
	}
}
