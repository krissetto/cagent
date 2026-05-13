package sidebar

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSubagents_OrderingStableAcrossHoverAndStatusCycles(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	now := time.Now()

	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "older-subagent-0001",
			ParentSessionID: "root-1",
			AgentName:       "planner",
			Status:          subagent.StatusRunning,
			CreatedAt:       now.Add(-2 * time.Minute),
		},
	})
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "newer-subagent-0002",
			ParentSessionID: "root-1",
			AgentName:       "reviewer",
			Status:          subagent.StatusRunning,
			CreatedAt:       now.Add(-1 * time.Minute),
		},
	})

	assertSubagentOrder := func(want ...string) {
		t.Helper()
		entries := m.subagentEntries()
		require.Len(t, entries, len(want))
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.ID)
		}
		assert.Equal(t, want, got)
	}

	// Newest delegation must come first regardless of subsequent activity.
	assertSubagentOrder("newer-subagent-0002", "older-subagent-0001")

	// Hover the older entry; ordering must not change.
	m.hoveredSubagentID = "older-subagent-0001"
	_ = m.subagentSection(80)
	assertSubagentOrder("newer-subagent-0002", "older-subagent-0001")

	// Status update on the older entry bumps UpdatedAt but must not
	// reorder rows under the user's cursor.
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID:      "older-subagent-0001",
			ParentSessionID: "root-1",
			AgentName:       "planner",
			Kind:            subagent.UpdateKindTurnCompleted,
			Status:          subagent.StatusWaiting,
			Preview:         "done",
			At:              now,
		},
	})
	assertSubagentOrder("newer-subagent-0002", "older-subagent-0001")

	// Sending fresh work to the older entry marks it running again and
	// touches UpdatedAt — still must not change order.
	m.recordSubAgentSent(&runtime.SubAgentSentEvent{
		SessionID:  "root-1",
		SubAgentID: "older-subagent-0001",
	})
	assertSubagentOrder("newer-subagent-0002", "older-subagent-0001")

	// Hover the newer entry to ensure rebuilds during render keep order.
	m.hoveredSubagentID = "newer-subagent-0002"
	_ = m.subagentSection(80)
	assertSubagentOrder("newer-subagent-0002", "older-subagent-0001")
}

// TestSubagents_SeedNestedSubagentReload verifies that seeding a multi-level
// session tree (root → A → B) puts the grandchild under its real parent with
// the correct agent name. This is the reload scenario: the runtime exposes a
// merged live + persisted tree and the sidebar must render it without losing
// nesting or agent identity.
func TestSubagents_SeedNestedSubagentReload(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		{ID: "root-1", Kind: runtime.LiveSessionRoot, AgentName: "root-agent", Status: "running"},
		{ID: "subagent-a", ParentID: "root-1", Kind: runtime.LiveSessionSubAgent, AgentName: "planner", Status: "closed", CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "subagent-b", ParentID: "subagent-a", Kind: runtime.LiveSessionSubAgent, AgentName: "reviewer", Status: "closed", CreatedAt: now.Add(-1 * time.Minute)},
	}

	m.SeedSubagentsFromLiveTree(nodes)

	b, ok := m.subagents["subagent-b"]
	require.True(t, ok, "nested grandchild should be seeded from the live tree")
	assert.Equal(t, "reviewer", b.AgentName,
		"agent name from the seed must survive verbatim, not be replaced by a placeholder")
	assert.Equal(t, "subagent-a", b.ParentSessionID)

	entries := m.subagentEntries()
	require.Len(t, entries, 2)
	depths := map[string]int{}
	for _, entry := range entries {
		depths[entry.ID] = m.subagentDepth(entry)
	}
	assert.Equal(t, 0, depths["subagent-a"])
	assert.Equal(t, 1, depths["subagent-b"],
		"the grandchild should render at depth 1 beneath its real parent")
}

// TestSubagents_SeedFromLiveTreePopulatesNestedDescendants verifies the seed
// path used when opening an attached sub-session tab onto a session that
// already has descendants running. Without this seed, sidebars for attached
// tabs would start empty and only begin tracking subagents as new events
// arrived — which is the exact "subsession sidebar not updating properly"
// bug the user reported.
func TestSubagents_SeedFromLiveTreePopulatesNestedDescendants(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	now := time.Now()
	nodes := []runtime.LiveSessionNode{
		// Root is the attached tab's own session. SeedSubagentsFromLiveTree
		// must ignore it — otherwise the sidebar would invent a phantom row.
		{ID: "worker-1", Kind: runtime.LiveSessionRoot, AgentName: "worker", Status: "running"},
		// Direct descendants.
		{ID: "planner-1", ParentID: "worker-1", Kind: runtime.LiveSessionSubAgent, AgentName: "planner", Status: "running", CreatedAt: now.Add(-3 * time.Minute), LastUpdateAt: now.Add(-1 * time.Minute)},
		{ID: "reviewer-1", ParentID: "worker-1", Kind: runtime.LiveSessionSubAgent, AgentName: "reviewer", Status: "waiting", CreatedAt: now.Add(-2 * time.Minute), LastPreview: "preview text"},
		// Grandchild under planner — this is the nested case that wouldn't
		// show up without seeding.
		{ID: "leaf-1", ParentID: "planner-1", Kind: runtime.LiveSessionSubAgent, AgentName: "leaf", Status: "failed", Error: "oops", CreatedAt: now.Add(-time.Minute)},
	}

	m.SeedSubagentsFromLiveTree(nodes)

	assert.Len(t, m.subagents, 3, "only subagent-kind nodes should be seeded; the root node must be ignored")
	assert.Contains(t, m.subagents, "planner-1")
	assert.Contains(t, m.subagents, "reviewer-1")
	assert.Contains(t, m.subagents, "leaf-1")

	planner := m.subagents["planner-1"]
	assert.Equal(t, "planner", planner.AgentName)
	assert.Equal(t, "worker-1", planner.ParentSessionID)
	assert.Equal(t, subagent.StatusRunning, planner.Status)

	leaf := m.subagents["leaf-1"]
	assert.Equal(t, "planner-1", leaf.ParentSessionID,
		"grandchildren must keep their real parent id so the sidebar tree renders correctly")
	assert.Equal(t, subagent.StatusFailed, leaf.Status)
	assert.Equal(t, "oops", leaf.Preview,
		"failed nodes should fall back to the error text as their preview since LastPreview is empty")

	entries := m.subagentEntries()
	require.Len(t, entries, 3)
	// planner (0) and reviewer (0) are depth-0 roots from the sidebar's
	// perspective; leaf is depth 1 because its parent planner is tracked.
	depths := map[string]int{}
	for _, e := range entries {
		depths[e.ID] = m.subagentDepth(e)
	}
	assert.Equal(t, 0, depths["planner-1"])
	assert.Equal(t, 0, depths["reviewer-1"])
	assert.Equal(t, 1, depths["leaf-1"],
		"leaf must be rendered as a child of planner, not as another root")

	// Re-seeding must not duplicate entries.
	m.SeedSubagentsFromLiveTree(nodes)
	assert.Len(t, m.subagents, 3, "seed is idempotent when called twice with the same snapshot")
}

func TestSubagents_SeedFromLiveTreeEmptyInputIsNoop(t *testing.T) {
	t.Parallel()
	m := newSubagentTestModel(t)
	m.SeedSubagentsFromLiveTree(nil)
	assert.Empty(t, m.subagents)
	m.SeedSubagentsFromLiveTree([]runtime.LiveSessionNode{})
	assert.Empty(t, m.subagents)
}

func TestSubagents_RecordStartAndRenderSection(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:        "0123456789abcdef",
			AgentName: "researcher",
			Title:     "Find citations",
			Status:    subagent.StatusRunning,
		},
	})

	require.Len(t, m.subagents, 1)
	state := m.subagents["0123456789abcdef"]
	require.NotNil(t, state)
	assert.Equal(t, "researcher", state.AgentName)
	assert.Equal(t, subagent.StatusRunning, state.Status)

	section := m.subagentSection(60)
	assert.Contains(t, section, "Subagents (1)")
	assert.Contains(t, section, "researcher")
	assert.Contains(t, section, subagent.ShortRef("0123456789abcdef"))
	assert.Contains(t, section, "working",
		"running subagents should show as 'working' in the sidebar label")
}

func TestSubagents_UpdateAppliesPreviewAndStatus(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SubAgent: subagent.HandleSnapshot{
			ID:        "abcdef1234567890",
			AgentName: "writer",
			Status:    subagent.StatusRunning,
		},
	})

	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: "abcdef1234567890",
			AgentName:  "writer",
			Kind:       subagent.UpdateKindTurnCompleted,
			Status:     subagent.StatusWaiting,
			Preview:    "Draft ready for review",
			At:         time.Now(),
		},
	})

	state := m.subagents["abcdef1234567890"]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusWaiting, state.Status)
	assert.Equal(t, "Draft ready for review", state.Preview)

	section := m.subagentSection(60)
	assert.Contains(t, section, "idle")
	assert.NotContains(t, section, "Draft ready for review",
		"successful turn previews must stay hidden — the sidebar only exposes the status chip")
}

func TestSubagents_StableOrderingWithEqualCreatedAt(t *testing.T) {
	t.Parallel()

	// After session reload, persisted CreatedAt timestamps come back at
	// second precision (RFC3339), so subagents started in the same second
	// share the exact same CreatedAt. The sort must still produce a
	// deterministic order — otherwise rows reshuffle on every re-render
	// because the surrounding map iteration is non-deterministic.

	shared := time.Now().Truncate(time.Second)
	ids := []string{
		"a-subagent-aaaaaaaa",
		"b-subagent-bbbbbbbb",
		"c-subagent-cccccccc",
		"d-subagent-dddddddd",
	}
	agents := []string{"coder", "planner", "reviewer", "writer"}

	var firstOrder []string
	for attempt := range 20 {
		m := newSubagentTestModel(t)
		// Insert in a different order on every attempt — Go's randomized
		// map iteration would otherwise eventually expose the instability
		// for us anyway, but this makes the test much more aggressive.
		for i := range ids {
			idx := (attempt + i) % len(ids)
			m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
				SessionID: "root-1",
				SubAgent: subagent.HandleSnapshot{
					ID:              ids[idx],
					ParentSessionID: "root-1",
					AgentName:       agents[idx],
					Status:          subagent.StatusRunning,
					CreatedAt:       shared,
				},
			})
		}

		entries := m.subagentEntries()
		require.Len(t, entries, len(ids))
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			got = append(got, e.ID)
		}

		if firstOrder == nil {
			firstOrder = got
			assert.Equal(t, []string{
				"a-subagent-aaaaaaaa", // coder
				"b-subagent-bbbbbbbb", // planner
				"c-subagent-cccccccc", // reviewer
				"d-subagent-dddddddd", // writer
			}, got, "equal CreatedAt values should be sorted alphabetically by agent name")
			continue
		}
		assert.Equal(t, firstOrder, got,
			"sibling order must be deterministic across re-renders even when CreatedAt collides")
	}
}

func TestSubagents_NewestDelegationFirstOrdering(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	now := time.Now()
	// Older delegation (started first).
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "live-old-000000",
			ParentSessionID: "root-1",
			AgentName:       "old-live",
			Status:          subagent.StatusRunning,
			CreatedAt:       now.Add(-5 * time.Minute),
		},
	})

	// Newer delegation (started second) but already closed.
	// It should still appear first because its CreatedAt is more recent.
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "recent-closed0",
			ParentSessionID: "root-1",
			AgentName:       "just-closed",
			Status:          subagent.StatusClosed,
			CreatedAt:       now.Add(-1 * time.Minute),
		},
	})

	// Simulate the older entry getting a fresh update — this must NOT
	// change the ordering because sort is by CreatedAt, not UpdatedAt.
	m.subagents["live-old-000000"].UpdatedAt = now

	entries := m.subagentEntries()
	require.Len(t, entries, 2)
	assert.Equal(t, "recent-closed0", entries[0].ID,
		"most recently delegated should appear first, regardless of UpdatedAt")
	assert.Equal(t, "live-old-000000", entries[1].ID)
}

func TestSubagents_LiveCountInTitleMixedStates(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "live-00000000",
			ParentSessionID: "root-1",
			AgentName:       "researcher",
			Status:          subagent.StatusRunning,
		},
	})
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "closed-0000000",
			ParentSessionID: "root-1",
			AgentName:       "reviewer",
			Status:          subagent.StatusRunning,
		},
	})
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID:      "closed-0000000",
			ParentSessionID: "root-1",
			AgentName:       "reviewer",
			Kind:            subagent.UpdateKindClosed,
			Status:          subagent.StatusClosed,
			At:              time.Now(),
		},
	})

	section := m.subagentSection(60)
	assert.Contains(t, section, "Subagents (1 live / 2)")
	assert.Equal(t, 1, m.liveSubagentCount())
	assert.Len(t, m.subagentEntries(), 2)
}

func TestSubagents_TreeOrderingAndDepth(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	now := time.Now()
	// Root child A (older update).
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent:  subagent.HandleSnapshot{ID: "aaaaa-1", ParentSessionID: "root-1", AgentName: "planner", Status: subagent.StatusRunning},
	})
	m.subagents["aaaaa-1"].UpdatedAt = now

	// Root child B — more recent update on the root sibling level.
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{SubAgentID: "bbbbb-1", ParentSessionID: "root-1", AgentName: "coder", Status: subagent.StatusWaiting, Kind: subagent.UpdateKindTurnCompleted, Preview: "b", At: now.Add(time.Second)},
	})

	// Descendants of A — these sit under aaaaa-1 regardless of their own
	// update time because they are a different sibling group (tree DFS).
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{SubAgentID: "ccccc-1", ParentSessionID: "aaaaa-1", AgentName: "reviewer", Status: subagent.StatusWaiting, Kind: subagent.UpdateKindTurnCompleted, Preview: "c", At: now.Add(2 * time.Second)},
	})
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{SubAgentID: "ddddd-1", ParentSessionID: "ccccc-1", AgentName: "leaf", Status: subagent.StatusWaiting, Kind: subagent.UpdateKindTurnCompleted, Preview: "d", At: now.Add(3 * time.Second)},
	})

	entries := m.subagentEntries()
	require.Len(t, entries, 4)
	assert.Equal(t, "bbbbb-1", entries[0].ID, "most recent root sibling should come first")
	assert.Equal(t, "aaaaa-1", entries[1].ID)
	assert.Equal(t, "ccccc-1", entries[2].ID, "child should follow its parent")
	assert.Equal(t, "ddddd-1", entries[3].ID, "grandchild should follow its direct parent")

	assert.Equal(t, 0, m.subagentDepth(entries[0]))
	assert.Equal(t, 0, m.subagentDepth(entries[1]))
	assert.Equal(t, 1, m.subagentDepth(entries[2]))
	assert.Equal(t, 2, m.subagentDepth(entries[3]))

	section := m.subagentSection(80)
	assert.Contains(t, section, "└ ", "nested tree should render closing connectors")
	assert.Contains(t, section, "leaf")
}

func TestSubagents_FailedUsesErrorAsPreview(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: "abcdef1234567890",
			AgentName:  "writer",
			Kind:       subagent.UpdateKindFailed,
			Status:     subagent.StatusFailed,
			Error:      "boom",
			At:         time.Now(),
		},
	})

	state := m.subagents["abcdef1234567890"]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusFailed, state.Status)
	assert.Equal(t, "boom", state.Preview)

	section := m.subagentSection(60)
	assert.Contains(t, section, "failed")
	assert.Contains(t, section, "boom")
}

func TestSubagents_EmptySectionReturnsEmpty(t *testing.T) {
	t.Parallel()
	m := newSubagentTestModel(t)
	assert.Empty(t, strings.TrimSpace(m.subagentSection(60)))
}

func TestSubagents_LiveSubagentKeepsSpinnerTicking(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	require.False(t, m.needsSpinner(), "fresh sidebar should not need a spinner")

	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SubAgent: subagent.HandleSnapshot{ID: "live-1", AgentName: "worker", Status: subagent.StatusRunning},
	})
	assert.True(t, m.needsSpinner(),
		"a running subagent must keep the shared sidebar spinner ticking so its row can animate")

	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: "live-1",
			AgentName:  "worker",
			Kind:       subagent.UpdateKindClosed,
			Status:     subagent.StatusClosed,
			At:         time.Now(),
		},
	})
	assert.False(t, m.needsSpinner(),
		"once every subagent has reached a terminal status the spinner is no longer required")
}

func TestSubagents_StreamCancelledDoesNotFinalizeLiveSubagents(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SubAgent: subagent.HandleSnapshot{ID: "x", AgentName: "y", Status: subagent.StatusRunning},
	})
	require.Len(t, m.subagents, 1)
	require.Equal(t, 1, m.liveSubagentCount())

	_, _ = m.Update(messages.StreamCancelledMsg{ShowMessage: true})
	assert.Len(t, m.subagents, 1,
		"ESC should stop parent-stream-local state but must not wipe live subagent sidebar state")
	state := m.subagents["x"]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusRunning, state.Status,
		"stream cancel must not invent terminal state for runtime-managed subagents")
	assert.Equal(t, 1, m.liveSubagentCount(),
		"live subagents should keep driving the shared sidebar spinner after parent cancel")
	assert.True(t, m.needsSpinner(),
		"the shared spinner should remain active while a subagent is still live")
	assert.True(t, m.spinnerActive, "startSpinner should have been called and set spinnerActive=true because a live subagent still exists")

	section := m.subagentSection(60)
	assert.Contains(t, section, "working")
	assert.NotContains(t, section, "finalized")
}

func TestSubagents_TerminalRuntimeEventStillFinalizesAfterCancel(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SubAgent: subagent.HandleSnapshot{ID: "x", AgentName: "y", Status: subagent.StatusRunning},
	})

	_, _ = m.Update(messages.StreamCancelledMsg{ShowMessage: true})
	_, _ = m.Update(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: "x",
			AgentName:  "y",
			Kind:       subagent.UpdateKindClosed,
			Status:     subagent.StatusClosed,
			At:         time.Now(),
		},
	})

	state := m.subagents["x"]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusClosed, state.Status,
		"runtime terminal events remain authoritative after parent cancel")
	assert.Equal(t, 0, m.liveSubagentCount())
	assert.False(t, m.needsSpinner())

	section := m.subagentSection(60)
	assert.Contains(t, section, "finalized")
	assert.NotContains(t, section, "working")
}

func TestSubagents_HoverShowsSessionStartedAge(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	start := time.Now().Add(-3 * time.Minute)
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "abcdef1234567890",
			ParentSessionID: "root-1",
			AgentName:       "researcher",
			Status:          subagent.StatusRunning,
			CreatedAt:       start,
		},
	})
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID:      "abcdef1234567890",
			ParentSessionID: "root-1",
			AgentName:       "researcher",
			Kind:            subagent.UpdateKindTurnCompleted,
			Status:          subagent.StatusWaiting,
			Preview:         "all done",
			At:              time.Now().Add(-10 * time.Second),
		},
	})

	section := m.subagentSection(60)
	assert.Contains(t, section, "idle")
	assert.NotContains(t, section, "ago ")

	m.hoveredSubagentID = "abcdef1234567890"
	hovered := m.subagentSection(60)
	assert.Contains(t, hovered, "3m ago")
	assert.NotContains(t, hovered, "10s ago")
}

func TestSubagents_HoverShowsRelativeAge(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)

	start := time.Now().Add(-3 * time.Minute)
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SessionID: "root-1",
		SubAgent: subagent.HandleSnapshot{
			ID:              "abcdef1234567890",
			ParentSessionID: "root-1",
			AgentName:       "researcher",
			Status:          subagent.StatusRunning,
			CreatedAt:       start,
		},
	})

	section := m.subagentSection(60)
	assert.Contains(t, section, "working")
	assert.NotContains(t, section, "3m ago")

	m.hoveredSubagentID = "abcdef1234567890"
	hovered := m.subagentSection(60)
	assert.Contains(t, hovered, "3m ago")
	assert.NotContains(t, hovered, "working")
}

func TestFormatRelativeAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero value becomes just now", time.Time{}, "just now"},
		{"sub-5s is just now", now.Add(-2 * time.Second), "just now"},
		{"under a minute", now.Add(-30 * time.Second), "30s ago"},
		{"under an hour", now.Add(-3 * time.Minute), "3m ago"},
		{"under a day", now.Add(-5 * time.Hour), "5h ago"},
		{"multiple days", now.Add(-49 * time.Hour), "2d ago"},
		{"future becomes just now", now.Add(10 * time.Second), "just now"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, formatRelativeAge(tc.t, now))
		})
	}
}

func newSubagentTestModel(t *testing.T) *model {
	t.Helper()
	sess := session.New()
	sessionState := service.NewSessionState(sess)
	return New(sessionState).(*model)
}

func TestSubagents_NoBlankLineBetweenRows(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	ids := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"}
	for i, id := range ids {
		m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
			SubAgent: subagent.HandleSnapshot{
				ID:        id,
				AgentName: fmt.Sprintf("worker-%d", i),
				Status:    subagent.StatusWaiting,
			},
		})
	}

	section := m.subagentSection(60)
	require.NotEmpty(t, section)

	// The body rows live directly after the tab header (title + top padding).
	// Each row must be non-blank and there must be no blank spacer between them.
	lines := strings.Split(section, "\n")
	const tabHeaderLines = 2
	require.Greater(t, len(lines), tabHeaderLines+len(ids)-1)
	for i := range ids {
		assert.NotEmpty(t, strings.TrimSpace(lines[tabHeaderLines+i]),
			"row %d should not be a blank spacer", i)
	}
}

func TestSubagents_ClickZonesMapBackToBackRows(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	ids := []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"}
	for i, id := range ids {
		m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
			SubAgent: subagent.HandleSnapshot{
				ID:        id,
				AgentName: fmt.Sprintf("worker-%d", i),
				Status:    subagent.StatusWaiting,
			},
		})
	}

	section := m.subagentSection(60)
	require.NotEmpty(t, section)

	// Build the lines slice the way renderSections does: the section's lines
	// follow the subagent section's start index. Two header lines come from
	// renderTab (title + top padding), then one line per entry with no blank
	// separators.
	sectionLines := strings.Split(section, "\n")
	lines := append([]string(nil), sectionLines...)
	m.buildSubagentClickZones(0, lines)

	// The renderer surfaces most-recent-first DFS order, which for inserts
	// performed in `ids` order means the rendered rows are reversed. Walk
	// the rendered entries directly to compare apples to apples.
	renderedIDs := make([]string, 0, len(ids))
	for _, e := range m.subagentRenderEntries() {
		renderedIDs = append(renderedIDs, e.state.ID)
	}
	require.ElementsMatch(t, ids, renderedIDs)

	const tabHeaderLines = 2
	for i, id := range renderedIDs {
		lineIdx := tabHeaderLines + i
		assert.Equal(t, id, m.subagentClickZones[lineIdx],
			"row %d should map directly to the matching subagent id without relying on a blank separator",
			i)
	}
}

func TestSubagents_NeverRendersAgentSelectionArrow(t *testing.T) {
	t.Parallel()

	// The ▶ glyph is reserved for the Agents section's "current agent"
	// row. The Subagents section must not render it for any lifecycle
	// state, otherwise the reader perceives it as a stale selection
	// indicator that should have been cleared on tab switch.
	m := newSubagentTestModel(t)
	cases := []struct {
		name   string
		status subagent.Status
	}{
		{"starting", subagent.StatusStarting},
		{"running", subagent.StatusRunning},
		{"waiting", subagent.StatusWaiting},
		{"closed", subagent.StatusClosed},
		{"stopped", subagent.StatusStopped},
		{"failed", subagent.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.subagents = map[string]*subagentState{
				"abcdef1234567890": {
					ID:        "abcdef1234567890",
					AgentName: "researcher",
					Status:    tc.status,
					CreatedAt: time.Now().Add(-time.Minute),
					UpdatedAt: time.Now(),
				},
			}
			section := m.subagentSection(60)
			assert.NotContains(t, section, "\u25b6",
				"the Subagents section must never render the \u25b6 selection arrow used by the Agents section")
		})
	}
}

func TestSubagents_LiveTreeCanonicalizesRestoredShortIDEntry(t *testing.T) {
	t.Parallel()

	// A restored chat seeds the sidebar with short-id keyed entries from
	// the parent transcript, then attaching to the live session calls
	// SeedSubagentsFromLiveTree with the full session id. We must not end
	// up with two rows for the same logical subagent.
	m := newSubagentTestModel(t)
	const fullID = "abcde1234567890ffeeddccbbaa"
	shortID := subagent.ShortRef(fullID)

	// Seed a stale, restore-style entry keyed by short id (status closed).
	m.subagents[shortID] = &subagentState{
		ID:        shortID,
		AgentName: "planner",
		Status:    subagent.StatusClosed,
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}

	// Now seed the live tree using the full session id with a TERMINAL
	// snapshot. A terminal live snapshot for the same subagent must keep
	// the row terminal (closed → closed is fine). The interesting fix —
	// a non-terminal live snapshot reviving a stale terminal row — is
	// covered by TestSubagents_LiveSnapshotRevivesStaleTerminalRow.
	m.SeedSubagentsFromLiveTree([]runtime.LiveSessionNode{
		{Kind: runtime.LiveSessionRoot, ID: "root"},
		{
			Kind:         runtime.LiveSessionSubAgent,
			ID:           fullID,
			ParentID:     "root",
			AgentName:    "planner",
			Status:       "closed",
			CreatedAt:    time.Now().Add(-time.Minute),
			LastUpdateAt: time.Now().Add(-time.Second),
		},
	})

	require.Len(t, m.subagents, 1,
		"a restored short-id entry must be canonicalized into the live full-id entry, not duplicated")
	state, ok := m.subagents[fullID]
	require.True(t, ok, "entry must end up keyed by the live full session id")
	assert.Equal(t, fullID, state.ID)
	assert.Equal(t, subagent.StatusClosed, state.Status,
		"terminal-on-terminal seed should keep the row closed")
	assert.Equal(t, "planner", state.AgentName)
}

// TestSubagents_LiveSnapshotRevivesStaleTerminalRow guards against a class
// of UX bugs where a subagent that is in fact still running (or waiting)
// shows up in the sidebar as `finalized`. That happened because the
// previous seed guard blanket-refused to revise a row whose UI status had
// already gone terminal — even when the runtime's own live tree was now
// reporting that subagent as live. Persisted-history snapshots can race
// ahead of live-manager snapshots during attach/reseed, leading to a
// terminal row that then refuses to recover.
//
// The fix is: a live-tree snapshot reporting a non-terminal status
// (starting / running / waiting) is authoritative and must replace any
// stale terminal UI state. A terminal snapshot is still treated as
// authoritative terminal information; that path is covered by the
// TestSubagents_LiveTreeCanonicalizesRestoredShortIDEntry test.
func TestSubagents_LiveSnapshotRevivesStaleTerminalRow(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	const id = "abcdef1234567890aaaa"

	// Pre-existing UI state says "closed" (stale: the runtime really has a
	// live handle for this subagent, the UI just doesn't know yet).
	m.subagents[id] = &subagentState{
		ID:        id,
		AgentName: "planner",
		Status:    subagent.StatusClosed,
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}

	// Live tree snapshot reports the same subagent as currently running.
	m.SeedSubagentsFromLiveTree([]runtime.LiveSessionNode{
		{Kind: runtime.LiveSessionRoot, ID: "root"},
		{
			Kind:         runtime.LiveSessionSubAgent,
			ID:           id,
			ParentID:     "root",
			AgentName:    "planner",
			Status:       "running",
			CreatedAt:    time.Now().Add(-time.Minute),
			LastUpdateAt: time.Now().Add(-time.Second),
		},
	})

	state := m.subagents[id]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusRunning, state.Status,
		"a live-tree snapshot reporting running must override stale terminal UI state — "+
			"this is what fixes 'subagent appears finalized while actually still running'")

	section := m.subagentSection(60)
	assert.Contains(t, section, "working",
		"sidebar label should reflect the revived live status, not the stale 'finalized'")
	assert.NotContains(t, section, "finalized",
		"the stale terminal label must no longer be rendered after a live revival snapshot")
}

// TestSubagents_LiveSnapshotPreservesTerminalOnTerminal confirms that the
// revival fix does not weaken the existing guard for genuinely terminal
// runtime state. Once the runtime itself reports the subagent as closed,
// the UI must continue to show terminal — a later identical snapshot must
// not flap the row back to non-terminal.
func TestSubagents_LiveSnapshotPreservesTerminalOnTerminal(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	const id = "abcdef1234567890bbbb"

	m.subagents[id] = &subagentState{
		ID:        id,
		AgentName: "planner",
		Status:    subagent.StatusClosed,
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}

	m.SeedSubagentsFromLiveTree([]runtime.LiveSessionNode{
		{Kind: runtime.LiveSessionRoot, ID: "root"},
		{
			Kind:         runtime.LiveSessionSubAgent,
			ID:           id,
			ParentID:     "root",
			AgentName:    "planner",
			Status:       "closed",
			CreatedAt:    time.Now().Add(-time.Minute),
			LastUpdateAt: time.Now().Add(-time.Second),
		},
	})

	state := m.subagents[id]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusClosed, state.Status,
		"terminal-on-terminal seed must remain terminal")
}

func TestSubagents_LiveTreeRendersGrandchildIndentedUnderChild(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	const rootID = "root-1"
	const childID = "child-aaaaaaaaaaaaaaaa"
	const grandchildID = "grandchild-bbbbbbbbbbbb"

	m.SeedSubagentsFromLiveTree([]runtime.LiveSessionNode{
		{Kind: runtime.LiveSessionRoot, ID: rootID},
		{
			Kind:         runtime.LiveSessionSubAgent,
			ID:           childID,
			ParentID:     rootID,
			AgentName:    "planner",
			Status:       "running",
			CreatedAt:    time.Now().Add(-2 * time.Minute),
			LastUpdateAt: time.Now().Add(-time.Minute),
		},
		{
			Kind:         runtime.LiveSessionSubAgent,
			ID:           grandchildID,
			ParentID:     childID,
			AgentName:    "researcher",
			Status:       "running",
			CreatedAt:    time.Now().Add(-time.Minute),
			LastUpdateAt: time.Now().Add(-30 * time.Second),
		},
	})

	entries := m.subagentRenderEntries()
	require.Len(t, entries, 2)
	assert.Equal(t, 0, entries[0].depth,
		"the direct child should render at depth 0 in the grandparent's sidebar")
	assert.Equal(t, childID, entries[0].state.ID)
	assert.Equal(t, 1, entries[1].depth,
		"the grandchild should render at depth 1 (indented under the child)")
	assert.Equal(t, grandchildID, entries[1].state.ID)
	assert.Equal(t, childID, entries[1].state.ParentSessionID)

	section := m.subagentSection(80)
	assert.Contains(t, section, "└",
		"nested grandchild row should render a tree connector")
}

func TestSubagents_HoverBrightensAgentName(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.recordSubAgentStart(&runtime.SubAgentStartedEvent{
		SubAgent: subagent.HandleSnapshot{
			ID:        "abcdef1234567890",
			AgentName: "researcher",
			Status:    subagent.StatusWaiting,
		},
	})

	notHovered := m.subagentSection(60)
	m.hoveredSubagentID = "abcdef1234567890"
	hovered := m.subagentSection(60)

	assert.NotEqual(t, notHovered, hovered,
		"hovering a subagent row should change the rendered output")
}

func TestParentSessionLine_HoverBrightensName(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("root-1"))
	ss := service.NewSessionState(sess)
	ss.SetParentAgentName("planner")
	sb := New(ss).(*model)

	notHovered := sb.parentSessionLine(60)
	sb.hoveredParentLine = true
	hovered := sb.parentSessionLine(60)

	assert.NotEqual(t, notHovered, hovered,
		"hovering the parent row should change the rendered output (brighter name + affordance)")
}

func TestSidebar_ClearTransientHoverResetsSubagentAndParentHover(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithParentID("root-1"))
	ss := service.NewSessionState(sess)
	ss.SetParentAgentName("planner")
	m := New(ss).(*model)
	m.hoveredSubagentID = "abcdef1234567890"
	m.hoveredParentLine = true
	m.cacheDirty = false

	m.ClearTransientHover()

	assert.Empty(t, m.hoveredSubagentID)
	assert.False(t, m.hoveredParentLine)
	assert.True(t, m.cacheDirty,
		"clearing hover state should invalidate cached rendering so the stale highlight disappears immediately")
}

func TestSubagents_SentMarksRunning(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	m.recordSubAgentUpdate(&runtime.SubAgentUpdateEvent{
		Envelope: subagent.Envelope{
			SubAgentID: "abcdef1234567890",
			AgentName:  "writer",
			Status:     subagent.StatusWaiting,
			Kind:       subagent.UpdateKindTurnCompleted,
			Preview:    "done",
			At:         time.Now().Add(-time.Minute),
		},
	})

	m.recordSubAgentSent(&runtime.SubAgentSentEvent{SubAgentID: "abcdef1234567890"})
	state := m.subagents["abcdef1234567890"]
	require.NotNil(t, state)
	assert.Equal(t, subagent.StatusRunning, state.Status,
		"sending a new message to an idle subagent should immediately mark it working again")
	assert.True(t, m.needsSpinner(),
		"a running subagent should keep the shared sidebar spinner active")
}
