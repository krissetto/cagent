package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

func TestNewHarnessProvider_RequiresRegistration(t *testing.T) {
	// Not parallel: swaps the package-level factory.
	original := harnessFactory.Load()
	harnessFactory.Store(nil)
	t.Cleanup(func() { harnessFactory.Store(original) })

	_, err := newHarnessProvider(&latest.HarnessConfig{Type: "codex"})
	require.ErrorIs(t, err, ErrHarnessNotRegistered)

	var called *latest.HarnessConfig
	RegisterHarness(func(cfg *latest.HarnessConfig) (harness.Provider, error) {
		called = cfg
		return nil, nil
	})
	_, err = newHarnessProvider(&latest.HarnessConfig{Type: "codex"})
	require.NoError(t, err)
	assert.Equal(t, "codex", called.Type)
}

func TestHarnessLabel(t *testing.T) {
	t.Parallel()
	assert.Empty(t, harnessLabel(nil))
	assert.Equal(t, "codex", harnessLabel(&latest.HarnessConfig{Type: "codex"}))
	assert.Equal(t, "claude-code/opus", harnessLabel(&latest.HarnessConfig{Type: "claude-code", Model: " opus "}))
}
