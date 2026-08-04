package root

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Non-regression for docker/docker-agent#3803: the debug command must be
// listed in the root help so users can discover it.
func TestDebug_ListedInRootHelp(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})

	require.NoError(t, root.Execute())

	help := out.String()
	assert.Contains(t, help, "Advanced Commands:")
	// Match the command listing line, not the -d/--debug flag.
	assert.Regexp(t, `(?m)^\s+debug\s+Debug tools$`, help)
}

func TestDebug_VisibleInAdvancedGroup(t *testing.T) {
	t.Parallel()

	cmd, _, err := NewRootCmd().Find([]string{"debug"})
	require.NoError(t, err)
	assert.Equal(t, "debug", cmd.Name())
	assert.False(t, cmd.Hidden)
	assert.Equal(t, "advanced", cmd.GroupID)
}
