package subagent

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type subagentStartArgs struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

type subagentSendArgs struct {
	SubagentID string `json:"subagent_id"`
	Message    string `json:"message"`
}

type subagentInspectArgs struct {
	SubagentID string `json:"subagent_id"`
	Mode       string `json:"mode"`
}

type subagentRefArgs struct {
	SubagentID string `json:"subagent_id"`
}

type model struct {
	*toolcommon.Base

	msg      *types.Message
	expanded bool
}

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	m := &model{msg: msg}
	m.Base = toolcommon.NewBase(msg, sessionState, func(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, height int) string {
		return m.render(msg, s, sessionState, width, height)
	})
	return m
}

func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := m.Base.Update(msg)
	return m, cmd
}

func (m *model) ToggleExpanded() bool {
	if !m.canExpand() {
		return false
	}
	m.expanded = !m.expanded
	return true
}

func (m *model) canExpand() bool {
	msg := m.msg
	return msg != nil && msg.ToolCall.Function.Name == runtime.ToolNameSubagentInspect && msg.ToolStatus == types.ToolStatusCompleted
}

func (m *model) SubAgentShortRef() string {
	if m == nil || m.msg == nil {
		return ""
	}
	return shortID(subAgentShortRefFromMessage(m.msg))
}

func (m *model) render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	var body string
	switch msg.ToolCall.Function.Name {
	case runtime.ToolNameSubagentStart:
		body = m.renderStart(msg)
	case runtime.ToolNameSubagentSend:
		body = m.renderSend(msg)
	case runtime.ToolNameSubagentInspect:
		body = m.renderInspect(msg, width)
	case runtime.ToolNameSubagentFinalize:
		body = m.renderFinalize(msg)
	case runtime.ToolNameSubagentStop:
		body = m.renderStop(msg)
	case runtime.ToolNameSubagentList:
		body = styles.MutedStyle.Render("listing runtime-managed subagents")
		if msg.ToolStatus == types.ToolStatusCompleted && strings.TrimSpace(msg.Content) != "" && !sessionState.HideToolResults() {
			body += "\n" + toolcommon.FormatToolResult(truncateLines(strings.TrimSpace(msg.Content), 8), width)
		}
	default:
		return toolcommon.RenderTool(msg, s, "", "", width, sessionState.HideToolResults())
	}
	return styles.ToolMessageStyle.Width(width).Render(toolcommon.Icon(msg, s) + " " + body)
}

func (m *model) renderStart(msg *types.Message) string {
	var args subagentStartArgs
	_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
	agent := strings.TrimSpace(args.Agent)
	if agent == "" {
		agent = extractString(msg.Content, "agent")
	}
	ref := ""
	if msg.ToolStatus == types.ToolStatusCompleted {
		ref = firstNonEmpty(extractString(msg.Content, "subagent_id"), extractString(msg.Content, "id"))
	}
	return renderSubagentAction("asking", agent, ref)
}

func (m *model) renderSend(msg *types.Message) string {
	var args subagentSendArgs
	_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
	ref := firstNonEmpty(extractString(msg.Content, "subagent_id"), strings.TrimSpace(args.SubagentID))
	return renderSubagentAction("replying to", extractString(msg.Content, "agent"), ref)
}

func (m *model) renderInspect(msg *types.Message, width int) string {
	var args subagentInspectArgs
	_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
	ref := strings.TrimSpace(args.SubagentID)
	if msg.ToolStatus != types.ToolStatusCompleted || strings.TrimSpace(msg.Content) == "" {
		return renderSubagentAction("inspecting", "", ref)
	}
	agent := extractString(msg.Content, "agent")
	ref = firstNonEmpty(extractString(msg.Content, "subagent_id"), ref)
	collapsed := renderSubagentAction("inspecting", agent, ref)
	if !m.expanded {
		return collapsed
	}
	last := firstNonEmpty(extractString(msg.Content, "last"), extractString(msg.Content, "latest_assistant_message"), extractString(msg.Content, "transcript"))
	if strings.TrimSpace(last) == "" {
		return collapsed
	}
	return collapsed + "\n" + styles.MutedStyle.Render(toolcommon.TruncateText(strings.TrimSpace(last), max(width-4, 24)))
}

func (m *model) renderFinalize(msg *types.Message) string {
	var args subagentRefArgs
	_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
	return styles.MutedStyle.Render("◇") + " " + renderSubagentAction("finalizing", extractString(msg.Content, "agent"), strings.TrimSpace(args.SubagentID))
}

func (m *model) renderStop(msg *types.Message) string {
	var args subagentRefArgs
	_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
	return styles.MutedStyle.Render("■") + " " + renderSubagentAction("stopping", extractString(msg.Content, "agent"), strings.TrimSpace(args.SubagentID))
}

func renderSubagentAction(verb, agentName, id string) string {
	verb = strings.TrimSpace(verb)
	agentName = strings.TrimSpace(agentName)
	id = shortID(strings.TrimSpace(id))
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
	switch msg.ToolCall.Function.Name {
	case runtime.ToolNameSubagentStart:
		return firstNonEmpty(extractString(msg.Content, "subagent_id"), extractString(msg.Content, "id"))
	case runtime.ToolNameSubagentSend:
		var args subagentSendArgs
		_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
		return firstNonEmpty(strings.TrimSpace(args.SubagentID), extractString(msg.Content, "subagent_id"))
	case runtime.ToolNameSubagentInspect:
		var args subagentInspectArgs
		_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
		return strings.TrimSpace(args.SubagentID)
	case runtime.ToolNameSubagentFinalize:
		var args subagentRefArgs
		_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
		return strings.TrimSpace(args.SubagentID)
	case runtime.ToolNameSubagentStop:
		var args subagentRefArgs
		_ = json.Unmarshal([]byte(msg.ToolCall.Function.Arguments), &args)
		return strings.TrimSpace(args.SubagentID)
	default:
		return ""
	}
}

func extractString(content, field string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err == nil {
		if v, ok := obj[field].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	for line := range strings.SplitSeq(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), field) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truncateLines(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n…"
}

func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 5 {
		return id
	}
	return id[:5]
}

var (
	_ layout.Model                           = (*model)(nil)
	_ interface{ ToggleExpanded() bool }     = (*model)(nil)
	_ interface{ SubAgentShortRef() string } = (*model)(nil)
)
