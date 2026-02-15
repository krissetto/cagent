
package messages

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/catppuccin/chatter/pkg/tui/common" // TODO: replace with actual module path
)

const longThreadText = `This is a long thread message with markdown.

# Heading

Lorem ipsum dolor sit amet, consectetur adipiscing elit. Donec quis lorem nec nulla faucibus.

- Item one
- Item two
- Item three
`

func BenchmarkMessagesView(b *testing.B) {
	messages := []common.Message{}
	for i := 0; i < 200; i++ {
		messages = append(messages, common.Message{
			Role:    common.MessageRoleAssistant,
			Content: longThreadText,
		})
	}

	component := New(Options{
		Renderer:     lipgloss.DefaultRenderer(),
		Width:        120,
		Height:       40,
		Theme:        common.DefaultTheme(),
		ShowReasoning: true,
	})

	component.SetMessages(messages)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = component.View()
	}
}
