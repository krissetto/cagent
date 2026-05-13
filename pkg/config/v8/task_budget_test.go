package v8

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskBudget_Unmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		yaml     string
		want     TaskBudget
		wantZero bool
		wantErr  bool
	}{
		{"integer shorthand", "128000\n", TaskBudget{Type: "tokens", Total: 128000}, false, false},
		{"zero shorthand disables", "0\n", TaskBudget{Type: "tokens", Total: 0}, true, false},
		{"full object", "type: tokens\ntotal: 64000\n", TaskBudget{Type: "tokens", Total: 64000}, false, false},
		{"zero object disables", "type: tokens\ntotal: 0\n", TaskBudget{Type: "tokens", Total: 0}, true, false},
		{"invalid type", "type: bogus\ntotal: 1\n", TaskBudget{}, false, true},
		{"negative total", "type: tokens\ntotal: -5\n", TaskBudget{}, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var tb TaskBudget
			err := yaml.Unmarshal([]byte(tc.yaml), &tb)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, tb)
			assert.Equal(t, tc.wantZero, tb.IsZero(),
				"IsZero mismatch for %q", tc.yaml)
			if tc.wantZero {
				assert.Nil(t, tb.AsMap(),
					"AsMap must return nil when budget is disabled (task_budget=0)")
			}
		})
	}
}

func TestTaskBudget_MarshalShorthand(t *testing.T) {
	t.Parallel()

	tb := TaskBudget{Type: "tokens", Total: 42}

	y, err := yaml.Marshal(&tb)
	require.NoError(t, err)
	assert.Equal(t, "42\n", string(y))

	j, err := json.Marshal(&tb)
	require.NoError(t, err)
	assert.JSONEq(t, `42`, string(j))
}

func TestTaskBudget_JSONRoundtrip(t *testing.T) {
	t.Parallel()

	orig := TaskBudget{Type: "tokens", Total: 200000}
	data, err := json.Marshal(&orig)
	require.NoError(t, err)

	var decoded TaskBudget
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, orig, decoded)
}

func TestTaskBudget_IsZeroAndAsMap(t *testing.T) {
	t.Parallel()

	var nilTB *TaskBudget
	assert.True(t, nilTB.IsZero())
	assert.Nil(t, nilTB.AsMap())

	assert.True(t, (&TaskBudget{}).IsZero())
	assert.Nil(t, (&TaskBudget{}).AsMap())

	// task_budget: 0 disables the feature: the integer shorthand produces
	// {Type: "tokens", Total: 0}, which must still be treated as zero.
	zero := &TaskBudget{Type: "tokens", Total: 0}
	assert.True(t, zero.IsZero(), "Total==0 must be treated as disabled")
	assert.Nil(t, zero.AsMap())

	tb := &TaskBudget{Total: 100}
	assert.False(t, tb.IsZero())
	assert.Equal(t, map[string]any{"type": "tokens", "total": 100}, tb.AsMap())
}

func TestModelConfig_TaskBudgetParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want TaskBudget
	}{
		{
			name: "integer shorthand",
			yaml: "provider: anthropic\nmodel: claude-opus-4-7\ntask_budget: 128000\n",
			want: TaskBudget{Type: "tokens", Total: 128000},
		},
		{
			name: "object form",
			yaml: "provider: anthropic\nmodel: claude-opus-4-7\ntask_budget:\n  type: tokens\n  total: 64000\n",
			want: TaskBudget{Type: "tokens", Total: 64000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var cfg ModelConfig
			require.NoError(t, yaml.Unmarshal([]byte(tc.yaml), &cfg))
			require.NotNil(t, cfg.TaskBudget)
			assert.Equal(t, tc.want, *cfg.TaskBudget)
		})
	}
}
