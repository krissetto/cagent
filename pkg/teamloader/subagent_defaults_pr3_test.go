package teamloader

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestLoadAppliesAgentLevelSubagentTTL(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
    subagents:
      - name: reviewer
        ttl: 42ms
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)
	require.NotNil(t, teams)

	root, err := teams.Agent("root")
	require.NoError(t, err)
	require.Len(t, root.SubAgentSpecs(), 1)
	assert.Equal(t, 42*time.Millisecond, root.SubAgentSpecs()[0].TTL)
	assert.NotZero(t, root.SubAgents())

	reviewer, err := teams.Agent("reviewer")
	require.NoError(t, err)
	assert.Zero(t, reviewer.IdleAutoFinalizeTimeout())
}
