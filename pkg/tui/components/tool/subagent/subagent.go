// Package subagenttool renders tool-call messages for the runtime-managed
// subagent tools (subagent_start / _send / _inspect / _finalize / _close /
// _stop / _list).
//
// The base presentation is deliberately compact so subagent traffic does not
// dominate the transcript.
package subagenttool

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	// subagent_inspect rows skip the action glyph (the legacy '?' marker)
	// because the verb "inspecting" already announces the action; the
	// shared tool-status icon (spinner / ✓) still prefixes the row so it
	// stays visually aligned with every other tool call.
	subagentFinalizeGlyph = "◇"
	subagentStopGlyph     = "■"
)

type model struct {
	*toolcommon.Base

	expanded bool
}

// New builds a compact renderer for subagent tool calls.
func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	m := &model{}
	m.Base = toolcommon.NewBase(msg, sessionState, func(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, height int) string {
		return m.render(msg, s, sessionState, width, height)
	})
	return m
}

func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Preserve the outer subagent model across updates. The embedded Base's
	// Update method returns the Base itself, which would erase this model's
	// extra behavior (notably SubAgentShortRef and inspect expansion) after the
	// first spinner tick or status update.
	_, cmd := m.Base.Update(msg)
	return m, cmd
}

// ToggleExpanded toggles the expanded/collapsed state of a completed
// subagent_inspect tool call. Returns true when the state actually changed.
func (m *model) ToggleExpanded() bool {
	if !m.canExpand() {
		return false
	}
	m.expanded = !m.expanded
	return true
}

func (m *model) canExpand() bool {
	msg := m.Message()
	return msg.ToolCall.Function.Name == subagent.ToolNameInspect && msg.ToolStatus == types.ToolStatusCompleted
}

// SubAgentShortRef returns the short subagent reference represented by this
// compact tool row, if any. The messages component uses it to turn a click on
// the row into an OpenSubAgentTab flow.
func (m *model) SubAgentShortRef() string {
	if m == nil || m.Message() == nil {
		return ""
	}
	return subAgentShortRefFromMessage(m.Message())
}

func (m *model) render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	// Not a recognised subagent tool? Fall back to the generic renderer.
	switch msg.ToolCall.Function.Name {
	case subagent.ToolNameStart,
		subagent.ToolNameSend,
		subagent.ToolNameInspect,
		subagent.ToolNameFinalize,
		subagent.ToolNameClose,
		subagent.ToolNameStop,
		subagent.ToolNameList:
	default:
		return toolcommon.RenderTool(msg, spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsAccentStyle), "", "", width, sessionState.HideToolResults())
	}

	var body string
	switch msg.ToolCall.Function.Name {
	case subagent.ToolNameStart:
		body = m.renderStart(msg)
	case subagent.ToolNameSend:
		body = m.renderSend(msg)
	case subagent.ToolNameInspect:
		body = m.renderInspect(msg, width)
	case subagent.ToolNameFinalize, subagent.ToolNameClose:
		body = m.renderFinalize(msg)
	case subagent.ToolNameStop:
		body = m.renderStop(msg)
	case subagent.ToolNameList:
		body = styles.MutedStyle.Render("List subagents")
	}

	// Prepend the shared tool-status icon so subagent rows share the same
	// left offset as every other tool call in the transcript; otherwise they
	// look visually dedented relative to their neighbours. No top margin is
	// applied here: the messages component already inserts a separator
	// between tool rows and adjacent non-tool content (see needsSeparator),
	// so an extra MarginTop(1) only compounds vertical padding.
	icon := toolcommon.Icon(msg, s)
	content := icon + " " + body
	return styles.ToolMessageStyle.Width(width).Render(content)
}

func (m *model) renderStart(msg *types.Message) string {
	params, _ := toolcommon.ParseArgs[subagent.StartArgs](msg.ToolCall.Function.Arguments)
	agentName := strings.TrimSpace(params.Agent)
	if agentName == "" {
		agentName = extractString(msg.Content, "agent")
	}
	shortID := ""
	if msg.ToolStatus == types.ToolStatusCompleted && strings.TrimSpace(msg.Content) != "" {
		shortID = extractString(msg.Content, "subagent_id")
	}
	return renderSubagentAction("asking", agentName, shortID)
}

func (m *model) renderSend(msg *types.Message) string {
	params, _ := toolcommon.ParseArgs[subagent.SendArgs](msg.ToolCall.Function.Arguments)
	agentName := ""
	if strings.TrimSpace(msg.Content) != "" {
		agentName = extractString(msg.Content, "agent")
	}
	ref := strings.TrimSpace(params.SubAgentID)
	if msg.ToolStatus == types.ToolStatusCompleted && strings.TrimSpace(msg.Content) != "" {
		if short := extractString(msg.Content, "subagent_id"); short != "" {
			ref = short
		}
	}
	return renderSubagentAction("replying to", agentName, ref)
}

func (m *model) renderInspect(msg *types.Message, width int) string {
	params, _ := toolcommon.ParseArgs[subagent.InspectArgs](msg.ToolCall.Function.Arguments)
	ref := strings.TrimSpace(params.SubAgentID)

	// Pending / running: render the verb up-front so the row reads as a clear
	// English action even before we know which agent it landed on.
	if msg.ToolStatus != types.ToolStatusCompleted || strings.TrimSpace(msg.Content) == "" {
		return renderSubagentAction("inspecting", "", ref)
	}

	var out struct {
		SubAgentID string `json:"subagent_id"`
		Agent      string `json:"agent"`
		Status     string `json:"status"`
		Last       string `json:"last"`
		Recent     []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"recent"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &out); err != nil {
		return renderSubagentAction("inspecting", "", ref)
	}

	// Completed: same shape as every other subagent row — `<glyph> inspecting [agent] (id)`.
	// The child's lifecycle status is intentionally not echoed here because the
	// ✓ tool-status icon already shows the inspect call succeeded; the child's
	// own state is surfaced by the sidebar / subagent section.
	collapsed := renderSubagentAction("inspecting", out.Agent, out.SubAgentID)

	if !m.expanded {
		return collapsed
	}

	lines := []string{collapsed}
	if strings.TrimSpace(out.Last) != "" {
		preview := toolcommon.TruncateText(strings.TrimSpace(out.Last), max(width-4, 24))
		lines = append(lines, styles.MutedStyle.Render(preview))
	}
	return strings.Join(lines, "\n")
}

func (m *model) renderFinalize(msg *types.Message) string {
	params, _ := toolcommon.ParseArgs[subagent.FinalizeArgs](msg.ToolCall.Function.Arguments)
	ref := strings.TrimSpace(params.SubAgentID)
	agent := ""
	if msg.ToolStatus == types.ToolStatusCompleted && strings.TrimSpace(msg.Content) != "" {
		agent = strings.TrimSpace(extractString(msg.Content, "agent"))
	}
	return actionPrefix(subagentFinalizeGlyph) + renderSubagentAction("finalizing", agent, ref)
}

func (m *model) renderStop(msg *types.Message) string {
	params, _ := toolcommon.ParseArgs[subagent.StopArgs](msg.ToolCall.Function.Arguments)
	ref := strings.TrimSpace(params.SubAgentID)
	agent := ""
	if msg.ToolStatus == types.ToolStatusCompleted && strings.TrimSpace(msg.Content) != "" {
		agent = strings.TrimSpace(extractString(msg.Content, "agent"))
	}
	return actionPrefix(subagentStopGlyph) + renderSubagentAction("stopping", agent, ref)
}

func actionPrefix(glyph string) string {
	return styles.MutedStyle.Render(glyph) + " "
}

func renderSubagentAction(verb, agentName, id string) string {
	verb = strings.TrimSpace(verb)
	agentName = strings.TrimSpace(agentName)
	id = strings.TrimSpace(id)

	parts := []string{styles.MutedStyle.Render(verb)}
	switch {
	case agentName != "" && id != "":
		parts = append(parts, styles.AgentBadgeStyleFor(agentName).Render(agentName)+styles.MutedStyle.Render(" ("+id+")"))
	case agentName != "":
		parts = append(parts, styles.AgentBadgeStyleFor(agentName).Render(agentName))
	case id != "":
		parts = append(parts, styles.MutedStyle.Render("("+id+")"))
	default:
		parts = append(parts, styles.MutedStyle.Render("subagent"))
	}
	return strings.Join(parts, " ")
}

func subAgentShortRefFromMessage(msg *types.Message) string {
	if msg == nil {
		return ""
	}

	switch msg.ToolCall.Function.Name {
	case subagent.ToolNameStart:
		if strings.TrimSpace(msg.Content) != "" {
			return strings.TrimSpace(extractString(msg.Content, "subagent_id"))
		}
		return ""
	case subagent.ToolNameSend:
		params, _ := toolcommon.ParseArgs[subagent.SendArgs](msg.ToolCall.Function.Arguments)
		if ref := strings.TrimSpace(params.SubAgentID); ref != "" {
			return ref
		}
		return strings.TrimSpace(extractString(msg.Content, "subagent_id"))
	case subagent.ToolNameInspect:
		params, _ := toolcommon.ParseArgs[subagent.InspectArgs](msg.ToolCall.Function.Arguments)
		return strings.TrimSpace(params.SubAgentID)
	case subagent.ToolNameFinalize, subagent.ToolNameClose:
		params, _ := toolcommon.ParseArgs[subagent.FinalizeArgs](msg.ToolCall.Function.Arguments)
		return strings.TrimSpace(params.SubAgentID)
	case subagent.ToolNameStop:
		params, _ := toolcommon.ParseArgs[subagent.StopArgs](msg.ToolCall.Function.Arguments)
		return strings.TrimSpace(params.SubAgentID)
	default:
		return ""
	}
}

func extractString(content, field string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return ""
	}
	if v, ok := obj[field].(string); ok {
		return v
	}
	return ""
}

var (
	_ layout.Model                           = (*model)(nil)
	_ interface{ ToggleExpanded() bool }     = (*model)(nil)
	_ interface{ SubAgentShortRef() string } = (*model)(nil)
)
