package message

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestAppendContentPreservesSplitImageOpenerAndExactContent(t *testing.T) {
	runtime := animation.NewRuntime()
	msg := types.Agent(types.MessageTypeAssistant, "root", "prefix ")
	m := New(runtime, msg, nil)
	for _, chunk := range []string{"!", "[alt]", "(https://example.com/image.png)", " suffix"} {
		_ = m.AppendContent(chunk)
	}
	require.Equal(t, "prefix ![alt](https://example.com/image.png) suffix", m.message.Content)
	require.Equal(t, "prefix ![alt](https://example.com/image.png) suffix", m.contentBuf.String())
}
