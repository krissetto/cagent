package toolexec

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/docker/docker-agent/pkg/tools"
)

// TestTranslateError_CallTimeout verifies the model-facing side of the
// call_timeout feature: a tool error wrapping tools.ErrCallTimeout is
// translated into an IsError tool result carrying the "timed out" message,
// without the generic "Error calling tool: ..." wrapping applied to other
// errors.
func TestTranslateError_CallTimeout(t *testing.T) {
	t.Parallel()

	c := &call{}
	_, span := noop.NewTracerProvider().Tracer("test").Start(t.Context(), "test")
	defer span.End()

	err := fmt.Errorf("%w after %s", tools.ErrCallTimeout, 60*time.Second)
	result := c.translateError(t.Context(), span, err)

	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Equal(t, "tool call timed out after 1m0s", result.Output)
}
