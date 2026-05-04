package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubagentsExclusivity verifies that presence of top-level `subagents:`
// (the sole opt-in for the runtime-managed subagent subsystem) is mutually
// exclusive with the legacy multi-agent surfaces on the same agent:
//
//   - `handoffs: [...]` (peer-to-peer routing flow)
//   - `- type: background_agents` (legacy parallel-task toolset)
//
// The former `- type: subagents` toolset opt-in is also no longer accepted
// and is rejected with an explicit migration error.
func TestSubagentsExclusivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "subagents alone is valid",
			yaml: `version: "9"
agents:
  root:
    model: openai/gpt-4o
    subagents: [helper]
  helper:
    model: openai/gpt-4o
`,
		},
		{
			name: "legacy sub_agents alias alone is valid",
			yaml: `version: "9"
agents:
  root:
    model: openai/gpt-4o
    sub_agents: [helper]
  helper:
    model: openai/gpt-4o
`,
		},
		{
			name: "explicit `- type: subagents` toolset is rejected with a migration hint",
			yaml: `version: "9"
agents:
  root:
    model: openai/gpt-4o
    subagents: [helper]
    toolsets:
      - type: subagents
  helper:
    model: openai/gpt-4o
`,
			wantErr: "`- type: subagents` is no longer valid",
		},
		{
			name: "subagents + handoffs is rejected",
			yaml: `version: "9"
agents:
  root:
    model: openai/gpt-4o
    subagents: [helper]
    handoffs: [peer]
  helper:
    model: openai/gpt-4o
  peer:
    model: openai/gpt-4o
`,
			wantErr: "cannot combine runtime-managed `subagents:` with `handoffs:`",
		},
		{
			name: "subagents + background_agents toolset is rejected",
			yaml: `version: "9"
agents:
  root:
    model: openai/gpt-4o
    subagents: [helper]
    toolsets:
      - type: background_agents
  helper:
    model: openai/gpt-4o
`,
			wantErr: "cannot combine runtime-managed `subagents:` with `- type: background_agents`",
		},
		{
			name: "handoffs alone (no subagents) is still allowed",
			yaml: `version: "9"
agents:
  root:
    model: openai/gpt-4o
    handoffs: [peer]
  peer:
    model: openai/gpt-4o
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(tc.yaml)))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
