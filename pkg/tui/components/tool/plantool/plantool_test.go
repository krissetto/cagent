package plantool

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

func planMessage(status types.ToolStatus, args, result string) *types.Message {
	return &types.Message{
		Type:       types.MessageTypeToolCall,
		ToolStatus: status,
		Content:    result,
		ToolCall: tools.ToolCall{
			Function: tools.FunctionCall{Arguments: args},
		},
		ToolDefinition: tools.Tool{
			Name:        "write_plan",
			Annotations: tools.ToolAnnotations{Title: "Write Plan"},
		},
	}
}

func render(t *testing.T, msg *types.Message) string {
	t.Helper()
	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	view.SetSize(120, 1)
	return stripANSI(view.View())
}

// TestPlanTool_SurfacesStatusAndTitle is the core requirement: a completed
// plan call shows the plan name (from args) and the status and title (from the
// result) next to each other.
func TestPlanTool_SurfacesStatusAndTitle(t *testing.T) {
	t.Parallel()

	out := render(t, planMessage(
		types.ToolStatusCompleted,
		`{"name":"release","content":"body"}`,
		`{"name":"release","title":"Release plan","status":"in-progress","revision":3}`,
	))

	assert.Contains(t, out, "release", "plan name should be shown")
	assert.Contains(t, out, "Release plan", "plan title should be surfaced")
	assert.Contains(t, out, "in-progress", "plan status should be surfaced")
	assert.Contains(t, out, "rev 3", "plan revision should be surfaced")
}

// TestPlanTool_StatusViewWithoutTitle covers get_plan_status / set_plan_status,
// whose lightweight result has a status and revision but no title.
func TestPlanTool_StatusViewWithoutTitle(t *testing.T) {
	t.Parallel()

	out := render(t, planMessage(
		types.ToolStatusCompleted,
		`{"name":"release"}`,
		`{"name":"release","status":"done","revision":5}`,
	))

	assert.Contains(t, out, "done")
	assert.Contains(t, out, "rev 5")
}

// TestPlanTool_ShowsConflictError verifies a version conflict (or any error) is
// surfaced verbatim, since that is what the reader must act on.
func TestPlanTool_ShowsConflictError(t *testing.T) {
	t.Parallel()

	out := render(t, planMessage(
		types.ToolStatusError,
		`{"name":"release","content":"x","last_known_revision":1}`,
		`version conflict on plan "release": last_known_revision 1 does not match current revision 4; re-read the plan and retry`,
	))

	assert.Contains(t, out, "version conflict")
	assert.Contains(t, out, "release")
}

func TestExtractName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "release", extractName(`{"name":"release","content":"x"}`))
	assert.Empty(t, extractName(`not json`))
	assert.Empty(t, extractName(`{"other":"field"}`))
}

func TestExtractSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status types.ToolStatus
		result string
		want   string
	}{
		{"full plan", types.ToolStatusCompleted, `{"title":"T","status":"draft","revision":2}`, `"T" [draft] rev 2`},
		{"status only", types.ToolStatusCompleted, `{"status":"done","revision":5}`, `[done] rev 5`},
		{"no status", types.ToolStatusCompleted, `{"title":"T","revision":1}`, `"T" rev 1`},
		{"empty result", types.ToolStatusCompleted, `{}`, ``},
		{"unparseable", types.ToolStatusCompleted, `not json`, ``},
		{"error passthrough", types.ToolStatusError, `boom`, `boom`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := &types.Message{ToolStatus: tc.status, Content: tc.result}
			assert.Equal(t, tc.want, extractSummary(msg))
		})
	}
}
