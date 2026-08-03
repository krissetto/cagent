package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvalContainerRuntimeFlagDefaultsToDocker(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()

	flag := cmd.Flags().Lookup("container-runtime")
	require.NotNil(t, flag, "eval must expose --container-runtime")
	assert.Equal(t, "docker", flag.DefValue)
}

func TestEvalContainerRuntimeFlagAcceptsCustomExecutable(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NoError(t, cmd.Flags().Parse([]string{"--container-runtime", "podman"}))

	value, err := cmd.Flags().GetString("container-runtime")
	require.NoError(t, err)
	assert.Equal(t, "podman", value)
}
