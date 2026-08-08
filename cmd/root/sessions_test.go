package root

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/replay"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func replaySession(toolName string) *session.Session {
	return &session.Session{Messages: []session.Item{
		{Message: &session.Message{Message: chat.Message{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{Function: tools.FunctionCall{Name: toolName, Arguments: "{}"}},
			},
		}}},
	}}
}

func TestRenderReplay_TextDivergence(t *testing.T) {
	t.Parallel()

	result := replay.CompareSessions(replaySession("read_file"), replaySession("shell"))
	var buf bytes.Buffer
	require.NoError(t, renderReplay(&buf, result, "aaa", "bbb", false))

	out := buf.String()
	assert.Contains(t, out, "First divergence at turn 0")
	assert.Contains(t, out, "aaa")
	assert.Contains(t, out, "bbb")
}

func TestRenderReplay_TextIdentical(t *testing.T) {
	t.Parallel()

	result := replay.CompareSessions(replaySession("read_file"), replaySession("read_file"))
	var buf bytes.Buffer
	require.NoError(t, renderReplay(&buf, result, "aaa", "bbb", false))
	assert.Contains(t, buf.String(), "Identical behaviour")
}

func TestRenderReplay_JSON(t *testing.T) {
	t.Parallel()

	result := replay.CompareSessions(replaySession("read_file"), replaySession("shell"))
	var buf bytes.Buffer
	require.NoError(t, renderReplay(&buf, result, "aaa", "bbb", true))

	var round replay.Result
	require.NoError(t, json.Unmarshal(buf.Bytes(), &round))
	require.NotNil(t, round.Divergence)
	assert.Equal(t, 0, round.Divergence.TurnIndex)
}

func TestReplayCmd_FlagsAreRegistered(t *testing.T) {
	t.Parallel()

	cmd := newReplayCmd()
	for _, name := range []string{"session-db", "json", "fail-on-divergence"} {
		assert.NotNilf(t, cmd.Flags().Lookup(name), "flag %q must exist", name)
	}
	// Two session IDs, no more, no fewer.
	require.Error(t, cmd.Args(cmd, []string{"only-one"}))
	require.NoError(t, cmd.Args(cmd, []string{"a", "b"}))
}
