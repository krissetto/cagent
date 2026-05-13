package subagenttool

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func stripANSI(s string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func TestInspectToggleExpanded(t *testing.T) {
	t.Parallel()

	msg := &types.Message{
		Type:       types.MessageTypeToolCall,
		Sender:     "root",
		ToolStatus: types.ToolStatusCompleted,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "subagent_inspect",
				Arguments: `{"subagent_id":"87120"}`,
			},
		},
		Content: `{"subagent_id":"87120","agent":"planner","status":"waiting","last":"hello there","recent":[{"role":"assistant","content":"x"}]}`,
	}

	m := New(msg, (*service.SessionState)(nil)).(*model)
	collapsed := m.View()
	require.NotEmpty(t, collapsed)
	assert.NotContains(t, collapsed, "hello there")
	assert.Contains(t, collapsed, "inspecting")
	assert.Contains(t, collapsed, "planner")
	assert.Contains(t, collapsed, "(87120)")
	assert.NotContains(t, collapsed, "?",
		"completed inspect rows must not show the legacy '?' action glyph; the shared tool-status icon already conveys completion")
	assert.NotContains(t, collapsed, "idle",
		"the child's lifecycle status no longer tails the inspect row — the sidebar is the canonical place for it")

	require.True(t, m.ToggleExpanded())
	expanded := m.View()
	assert.Contains(t, expanded, "hello there")
	assert.Contains(t, expanded, "planner")
}

func TestStartPendingShowsDelegationImmediatelyWithoutParentPill(t *testing.T) {
	t.Parallel()

	msg := &types.Message{
		Type:       types.MessageTypeToolCall,
		Sender:     "root",
		ToolStatus: types.ToolStatusPending,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "subagent_start",
				Arguments: `{"agent":"planner"`,
			},
		},
	}

	view := New(msg, (*service.SessionState)(nil)).(*model).View()
	assert.Contains(t, view, "asking")
	assert.Contains(t, view, "planner")
	assert.NotContains(t, view, "→",
		"asking rows must no longer prepend the legacy '→' glyph; the spinner/check icon already conveys status")
	assert.NotContains(t, view, "root",
		"delegation rows should no longer render the current parent agent pill in chat")
}

func TestStartCompletedShowsChildAndShortIDWithoutParentPill(t *testing.T) {
	t.Parallel()

	msg := &types.Message{
		Type:       types.MessageTypeToolCall,
		Sender:     "root",
		ToolStatus: types.ToolStatusCompleted,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "subagent_start",
				Arguments: `{"agent":"planner","task":"plan it"}`,
			},
		},
		Content: `{"subagent_id":"87120","agent":"planner","status":"running"}`,
	}

	view := New(msg, (*service.SessionState)(nil)).(*model).View()
	assert.Contains(t, view, "asking")
	assert.Contains(t, view, "planner")
	assert.Contains(t, view, "(87120)")
	assert.NotContains(t, view, "→")
	assert.NotContains(t, view, "root")
}

func TestSubAgentShortRefSourcesPerTool(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		msg      *types.Message
		wantRef  string
		wantKind string
	}{
		{
			name: "start pending has no ref yet",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusPending,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_start",
					Arguments: `{"agent":"planner"`,
				}},
			},
			wantRef: "",
		},
		{
			name: "start completed reads ref from tool content",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusCompleted,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_start",
					Arguments: `{"agent":"planner","task":"plan it"}`,
				}},
				Content: `{"subagent_id":"87120","agent":"planner","status":"running"}`,
			},
			wantRef: "87120",
		},
		{
			name: "send reads ref from args",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusPending,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_send",
					Arguments: `{"subagent_id":"abcde","message":"hi"}`,
				}},
			},
			wantRef: "abcde",
		},
		{
			name: "inspect reads ref from args",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusCompleted,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_inspect",
					Arguments: `{"subagent_id":"abcde"}`,
				}},
				Content: `{"subagent_id":"abcde","agent":"planner","status":"waiting"}`,
			},
			wantRef: "abcde",
		},
		{
			name: "finalize reads ref from args",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusPending,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_finalize",
					Arguments: `{"subagent_id":"abcde"}`,
				}},
			},
			wantRef: "abcde",
		},
		{
			name: "stop reads ref from args",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusPending,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_stop",
					Arguments: `{"subagent_id":"abcde"}`,
				}},
			},
			wantRef: "abcde",
		},
		{
			name: "list has no specific ref",
			msg: &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusCompleted,
				ToolCall: tools.ToolCall{Function: tools.FunctionCall{
					Name:      "subagent_list",
					Arguments: `{}`,
				}},
			},
			wantRef: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := New(tc.msg, (*service.SessionState)(nil)).(*model)
			assert.Equal(t, tc.wantRef, m.SubAgentShortRef())
		})
	}
}

func TestSendPendingShowsReplyActionImmediately(t *testing.T) {
	t.Parallel()

	msg := &types.Message{
		Type:       types.MessageTypeToolCall,
		Sender:     "root",
		ToolStatus: types.ToolStatusPending,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "subagent_send",
				Arguments: `{"subagent_id":"87120","message":"hi"`,
			},
		},
	}

	view := New(msg, (*service.SessionState)(nil)).(*model).View()
	assert.Contains(t, view, "replying to")
	assert.Contains(t, view, "(87120)")
	assert.NotContains(t, view, "↪",
		"reply rows must no longer prepend the legacy '↪' arrow; the spinner/check status icon already conveys progress")
}

func TestSendCompletedUsesReplyArrowAndAgentBadge(t *testing.T) {
	t.Parallel()

	msg := &types.Message{
		Type:       types.MessageTypeToolCall,
		Sender:     "root",
		ToolStatus: types.ToolStatusCompleted,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "subagent_send",
				Arguments: `{"subagent_id":"87120","message":"hi"}`,
			},
		},
		Content: `{"subagent_id":"87120","agent":"planner","status":"running"}`,
	}

	view := New(msg, (*service.SessionState)(nil)).(*model).View()
	assert.Contains(t, view, "replying to")
	assert.Contains(t, view, "planner")
	assert.Contains(t, view, "(87120)")
	assert.NotContains(t, view, "↪")
	assert.NotContains(t, view, "root",
		"reply rows should render as 'replying to [child] (id)', not as parent -> child again")
}

func TestFinalizeAndStopUseDistinctActionGlyphs(t *testing.T) {
	t.Parallel()

	t.Run("finalize", func(t *testing.T) {
		msg := &types.Message{
			Type:       types.MessageTypeToolCall,
			ToolStatus: types.ToolStatusPending,
			ToolCall: tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      "subagent_finalize",
					Arguments: `{"subagent_id":"87120"}`,
				},
			},
		}

		view := New(msg, (*service.SessionState)(nil)).(*model).View()
		assert.Contains(t, view, subagentFinalizeGlyph)
		assert.Contains(t, view, "finalizing")
		assert.Contains(t, view, "(87120)")
	})

	t.Run("finalize completed surfaces the agent badge", func(t *testing.T) {
		// When the finalize tool call completes, the runtime response carries
		// the child agent name; the transcript row should then mirror the
		// delegation/send rows as `<glyph> [agent] · <id>`.
		msg := &types.Message{
			Type:       types.MessageTypeToolCall,
			ToolStatus: types.ToolStatusCompleted,
			ToolCall: tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      "subagent_finalize",
					Arguments: `{"subagent_id":"87120"}`,
				},
			},
			Content: `{"subagent_id":"87120","agent":"planner","status":"closed"}`,
		}

		view := New(msg, (*service.SessionState)(nil)).(*model).View()
		assert.Contains(t, view, subagentFinalizeGlyph)
		assert.Contains(t, view, "finalizing")
		assert.Contains(t, view, "planner",
			"finalize completion should show the child's agent badge when the content exposes it")
		assert.Contains(t, view, "(87120)")
	})

	t.Run("stop", func(t *testing.T) {
		msg := &types.Message{
			Type:       types.MessageTypeToolCall,
			ToolStatus: types.ToolStatusPending,
			ToolCall: tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      "subagent_stop",
					Arguments: `{"subagent_id":"87120"}`,
				},
			},
		}

		view := New(msg, (*service.SessionState)(nil)).(*model).View()
		assert.Contains(t, view, subagentStopGlyph)
		assert.Contains(t, view, "stopping")
		assert.Contains(t, view, "(87120)")
	})

	t.Run("stop completed surfaces the agent badge", func(t *testing.T) {
		msg := &types.Message{
			Type:       types.MessageTypeToolCall,
			ToolStatus: types.ToolStatusCompleted,
			ToolCall: tools.ToolCall{
				Function: tools.FunctionCall{
					Name:      "subagent_stop",
					Arguments: `{"subagent_id":"87120"}`,
				},
			},
			Content: `{"subagent_id":"87120","agent":"planner","status":"stopped"}`,
		}

		view := New(msg, (*service.SessionState)(nil)).(*model).View()
		assert.Contains(t, view, subagentStopGlyph)
		assert.Contains(t, view, "stopping")
		assert.Contains(t, view, "planner")
		assert.Contains(t, view, "(87120)")
	})
}

func TestInspectPendingUsesCompactQueryShape(t *testing.T) {
	t.Parallel()

	msg := &types.Message{
		Type:       types.MessageTypeToolCall,
		ToolStatus: types.ToolStatusPending,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      "subagent_inspect",
				Arguments: `{"subagent_id":"87120"}`,
			},
		},
	}

	view := New(msg, (*service.SessionState)(nil)).(*model).View()
	assert.Contains(t, view, "inspecting")
	assert.Contains(t, view, "(87120)")
	assert.NotContains(t, view, "?",
		"pending inspect rows must not render the legacy '?' action glyph")
	assert.NotContains(t, view, "inspect…",
		"pending inspect rows should stay as terse as the delegation/send rows")
}

// TestSubagentRowsShareCompletedLeftIndent locks in the visual rule that every
// completed subagent tool row starts at the same column as the other tool
// calls. The shared toolcommon.Icon prefix gives a `"  \u2713 "` indent (two
// spaces, the check, one space), so the row body's first non-space rune must
// always sit at column 4 — regardless of which subagent verb the row uses.
func TestSubagentRowsShareCompletedLeftIndent(t *testing.T) {
	t.Parallel()

	content := `{"subagent_id":"87120","agent":"planner","status":"running","last":"hi"}`
	cases := []struct {
		name string
		tool string
		args string
		verb string
	}{
		{"start", "subagent_start", `{"agent":"planner"}`, "asking"},
		{"send", "subagent_send", `{"subagent_id":"87120","message":"hi"}`, "replying to"},
		{"inspect", "subagent_inspect", `{"subagent_id":"87120"}`, "inspecting"},
	}
	indents := make(map[string]int, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusCompleted,
				ToolCall: tools.ToolCall{
					Function: tools.FunctionCall{Name: tc.tool, Arguments: tc.args},
				},
				Content: content,
			}
			view := stripANSI(New(msg, (*service.SessionState)(nil)).(*model).View())
			idx := strings.Index(view, tc.verb)
			require.GreaterOrEqual(t, idx, 0, "verb must be present")
			// Walk back to the start of the visible line.
			lineStart := strings.LastIndex(view[:idx], "\n") + 1
			indent := idx - lineStart
			indents[tc.name] = indent
		})
	}
	require.Equal(t, indents["start"], indents["send"], "start and send rows must share the left indent")
	require.Equal(t, indents["start"], indents["inspect"], "inspect rows must share the same left indent as other subagent rows")
}

// TestSubagentRowSurvivesUpdateRoundTripForClickability locks in the rule that
// the layout view stored after Update() preserves the SubAgentShortRef
// contract. Without an explicit Update override on the subagent model, the
// embedded toolcommon.Base.Update would return the Base itself and silently
// strip the click target after the very first tick, breaking re-attach for
// `replying to` and `inspecting` rows.
func TestSubagentRowSurvivesUpdateRoundTripForClickability(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"subagent_send", "subagent_inspect"} {
		t.Run(tool, func(t *testing.T) {
			msg := &types.Message{
				Type:       types.MessageTypeToolCall,
				ToolStatus: types.ToolStatusCompleted,
				ToolCall: tools.ToolCall{
					Function: tools.FunctionCall{Name: tool, Arguments: `{"subagent_id":"abcde"}`},
				},
				Content: `{"subagent_id":"abcde","agent":"planner","status":"running","last":"hi"}`,
			}
			view := New(msg, (*service.SessionState)(nil))
			updated, _ := view.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			refProvider, ok := updated.(interface{ SubAgentShortRef() string })
			require.True(t, ok, "updated view must still expose SubAgentShortRef()")
			assert.Equal(t, "abcde", refProvider.SubAgentShortRef())
		})
	}
}
