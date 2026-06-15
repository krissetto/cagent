package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func invariantSystemText(a *agent.Agent) string {
	msgs := buildInvariantSystemMessages(a)
	parts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role == chat.MessageRoleSystem {
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, "\n---\n")
}

func TestRuntimeManagedSubagentPromptGatedSeparatelyFromTransferPrompt(t *testing.T) {
	t.Parallel()

	child := agent.New("implementer", "", agent.WithDescription("edits code"))
	transfer := agent.New("reviewer", "", agent.WithDescription("reviews code"))

	tests := []struct {
		name                string
		agent               *agent.Agent
		wantRuntimeGuidance bool
		wantTransferPrompt  bool
	}{
		{
			name:  "none",
			agent: agent.New("root", ""),
		},
		{
			name: "subagents-only",
			agent: func() *agent.Agent {
				a := agent.New("root", "")
				agent.WithSubAgents(child)(a)
				return a
			}(),
			wantRuntimeGuidance: true,
		},
		{
			name: "transfer-only",
			agent: func() *agent.Agent {
				a := agent.New("root", "")
				agent.WithTransferAgents(transfer)(a)
				return a
			}(),
			wantTransferPrompt: true,
		},
		{
			name: "both",
			agent: func() *agent.Agent {
				a := agent.New("root", "")
				agent.WithSubAgents(child)(a)
				agent.WithTransferAgents(transfer)(a)
				return a
			}(),
			wantRuntimeGuidance: true,
			wantTransferPrompt:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := invariantSystemText(tt.agent)
			if tt.wantRuntimeGuidance {
				assert.Contains(t, text, "runtime-managed subagents")
				assert.Contains(t, text, "end your turn and wait")
				assert.Contains(t, text, "Do not poll for progress")
				assert.Contains(t, text, "do not call `subagent_inspect` or `subagent_list` just to check whether the subagent is done")
				assert.Contains(t, text, "automatic subagent update/result envelope")
			} else {
				assert.NotContains(t, text, "runtime-managed subagents")
				assert.NotContains(t, text, "Do not poll for progress")
				assert.NotContains(t, text, "subagent_inspect")
			}

			if tt.wantTransferPrompt {
				assert.Contains(t, text, "transfer_task")
				assert.Contains(t, text, "valid agent names")
			} else {
				if tt.wantRuntimeGuidance {
					assert.NotContains(t, text, "call `transfer_task` function")
				} else {
					assert.NotContains(t, text, "transfer_task")
				}
			}
		})
	}
}

func TestRuntimeManagedSubagentPromptListsConfiguredSpecAliases(t *testing.T) {
	t.Parallel()

	coder := agent.New("coder", "", agent.WithDescription("fallback description"))
	root := agent.New("root", "")
	agent.WithSubAgents(coder)(root)
	agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "implementer", Agent: "coder", Description: "bench maker"})(root)

	text := invariantSystemText(root)
	require.Contains(t, text, "Name: implementer | Description: bench maker")
	assert.Contains(t, text, "valid subagent names are: implementer")
}
