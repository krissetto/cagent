package teamloader

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestLoadAppliesPerSubagentIdleAutoFinalizeTimeout(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	yaml := []byte(`
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: root
  reviewer:
    model: openai/gpt-4o-mini
    instruction: reviewer
subagents:
  reviewer:
    idle_auto_finalize_timeout: 42ms
`)

	teams, err := Load(t.Context(), config.NewBytesSource("agent.yaml", yaml), &config.RuntimeConfig{})
	require.NoError(t, err)
	require.NotNil(t, teams)

	reviewer, err := teams.Agent("reviewer")
	require.NoError(t, err)
	assert.Equal(t, 42*time.Millisecond, reviewer.IdleAutoFinalizeTimeout())

	root, err := teams.Agent("root")
	require.NoError(t, err)
	assert.Zero(t, root.IdleAutoFinalizeTimeout())
}
