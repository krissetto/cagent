package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateInspectMessages_BoundaryCommaAccounting(t *testing.T) {
	t.Parallel()

	// Build two messages whose marshalled sizes we know exactly.
	msg0 := map[string]string{"role": "user", "content": strings.Repeat("a", 100)}
	msg1 := map[string]string{"role": "assistant", "content": "short"}

	// Compute the exact JSON array size of [msg0,msg1] and [msg1].
	full, _ := json.Marshal([]map[string]string{msg0, msg1})
	single, _ := json.Marshal([]map[string]string{msg1})

	// Boundary: maxBytes == len(single) — omitting msg0 must make it fit.
	trimmed, omitted, truncated := truncateInspectMessages(
		[]map[string]string{msg0, msg1},
		len(single),
	)
	require.True(t, truncated, "should truncate when full array exceeds maxBytes")
	assert.Equal(t, 1, omitted, "should omit exactly msg0")
	assert.Len(t, trimmed, 1, "should return exactly msg1")
	assert.Equal(t, "short", trimmed[0]["content"])

	// Verify the result actually fits within maxBytes.
	resultArray, _ := json.Marshal([]map[string]string(trimmed))
	assert.LessOrEqual(t, len(resultArray), len(single),
		"result array JSON must fit within maxBytes")

	// No truncation needed when maxBytes is generous.
	trimmed, omitted, truncated = truncateInspectMessages(
		[]map[string]string{msg0, msg1},
		len(full),
	)
	assert.False(t, truncated)
	assert.Equal(t, 0, omitted)
	assert.Len(t, trimmed, 2)

	// Everything omitted when maxBytes is tiny.
	trimmed, omitted, truncated = truncateInspectMessages(
		[]map[string]string{msg0, msg1},
		1,
	)
	assert.True(t, truncated)
	assert.Equal(t, 2, omitted)
	assert.Empty(t, trimmed)

	// Nil / empty inputs.
	trimmed, omitted, truncated = truncateInspectMessages(nil, 100)
	assert.False(t, truncated)
	assert.Equal(t, 0, omitted)
	assert.Nil(t, trimmed)
}

func TestTruncateInspectMessages_ManyMessages(t *testing.T) {
	t.Parallel()

	// Build 20 messages of ~200 bytes each.
	msgs := make([]map[string]string, 20)
	for i := range msgs {
		msgs[i] = map[string]string{
			"role":    "assistant",
			"content": fmt.Sprintf("msg-%02d %s", i, strings.Repeat("x", 180)),
		}
	}

	// Allow roughly half.
	fullJSON, _ := json.Marshal(msgs)
	maxBytes := len(fullJSON) / 2

	trimmed, omitted, truncated := truncateInspectMessages(msgs, maxBytes)
	require.True(t, truncated)
	require.Greater(t, omitted, 0)
	require.Less(t, omitted, len(msgs))

	// Verify result fits.
	resultJSON, _ := json.Marshal(trimmed)
	assert.LessOrEqual(t, len(resultJSON), maxBytes,
		"truncated result must fit within maxBytes (got %d, max %d)", len(resultJSON), maxBytes)
}
