package subagent

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolSetDoesNotAdvertiseDeprecatedCloseAlias(t *testing.T) {
	t.Parallel()

	ts := NewToolSet()
	tools, err := ts.Tools(t.Context())
	require.NoError(t, err)

	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	assert.Contains(t, names, ToolNameFinalize)
	assert.NotContains(t, names, ToolNameClose,
		"deprecated close alias should remain dispatchable but not advertised to the model")
}

func TestSubagentIDSchemaDescriptionsStayInSync(t *testing.T) {
	t.Parallel()

	types := []reflect.Type{
		reflect.TypeFor[SendArgs](),
		reflect.TypeFor[InspectArgs](),
		reflect.TypeFor[FinalizeArgs](),
		reflect.TypeFor[StopArgs](),
	}
	for _, typ := range types {
		field, ok := typ.FieldByName("SubAgentID")
		require.True(t, ok)
		assert.Equal(t, subagentIDDescription, field.Tag.Get("jsonschema"), typ.Name())
	}
}
