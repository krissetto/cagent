package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewIDIsFiveHexChars(t *testing.T) {
	id := string(NewID())
	assert.Len(t, id, 5)
	assert.Regexp(t, `^[0-9a-f]{5}$`, id)
}

func TestTreeNewNodeIDAvoidsExistingIDs(t *testing.T) {
	tr := NewTree()
	id := tr.NewNodeID()
	err := tr.Add(Node{ID: id, Agent: "a"})
	require.NoError(t, err)

	for range 100 {
		next := tr.NewNodeID()
		assert.NotEqual(t, id, next)
		assert.Len(t, string(next), 5)
	}
}
