package message

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/tui/types"
)

func TestMessageCaching_BasicFunctionality(t *testing.T) {
	msg := &types.Message{
		Type:    types.MessageTypeAssistant,
		Content: "Test message with **bold** text",
	}
	mv := New(msg)
	mv.SetSize(80, 0)

	// First render should compute and cache
	view1 := mv.View()
	require.NotEmpty(t, view1)
	assert.NotEmpty(t, mv.cachedView, "Cache should be populated after first render")
	assert.Equal(t, 80, mv.cachedWidth, "Cached width should match")

	// Second render should use cache
	view2 := mv.View()
	assert.Equal(t, view1, view2, "Cached view should be identical")
}

func TestMessageCaching_InvalidateOnWidthChange(t *testing.T) {
	msg := &types.Message{
		Type:    types.MessageTypeAssistant,
		Content: "Test message with **bold** text that wraps",
	}
	mv := New(msg)
	mv.SetSize(80, 0)

	// Initial render
	view1 := mv.View()
	require.NotEmpty(t, view1)
	cachedView1 := mv.cachedView

	// Change width should invalidate cache
	mv.SetSize(120, 0)
	assert.Empty(t, mv.cachedView, "Cache should be invalidated on width change")

	// New render with new width
	view2 := mv.View()
	require.NotEmpty(t, view2)
	assert.NotEmpty(t, mv.cachedView, "Cache should be repopulated")
	assert.NotEqual(t, cachedView1, mv.cachedView, "Cached content should differ after width change")
}

func TestMessageCaching_InvalidateOnContentChange(t *testing.T) {
	msg := &types.Message{
		Type:    types.MessageTypeAssistant,
		Content: "Original content",
	}
	mv := New(msg)
	mv.SetSize(80, 0)

	// Initial render
	view1 := mv.View()
	require.NotEmpty(t, view1)

	// Change content
	msg.Content = "New content"
	mv.SetMessage(msg)
	assert.Empty(t, mv.cachedView, "Cache should be invalidated on message change")

	// New render
	view2 := mv.View()
	assert.NotEqual(t, view1, view2, "View should differ after content change")
}

func TestMessageCaching_NoSideEffects(t *testing.T) {
	msg := &types.Message{
		Type:    types.MessageTypeUser,
		Content: "Test message",
	}
	mv := New(msg)
	mv.SetSize(80, 0)

	// Multiple renders should produce identical output
	views := make([]string, 5)
	for i := range views {
		views[i] = mv.View()
	}

	for i := range views {
		assert.Equal(t, views[0], views[i], "Multiple renders should produce identical output")
	}
}

func TestMessageCaching_SpinnerNotCached(t *testing.T) {
	msg := &types.Message{
		Type: types.MessageTypeSpinner,
	}
	mv := New(msg)
	mv.SetSize(80, 0)
	mv.Init()

	// Spinner messages should not be cached
	_ = mv.View()
	assert.Empty(t, mv.cachedView, "Spinner messages should not be cached")
}

func TestMessageCaching_EmptyContentNotCached(t *testing.T) {
	msg := &types.Message{
		Type:    types.MessageTypeAssistant,
		Content: "",
	}
	mv := New(msg)
	mv.SetSize(80, 0)

	// Empty content should not be cached
	_ = mv.View()
	assert.Empty(t, mv.cachedView, "Empty content should not be cached")
}

func TestMessageCaching_AllMessageTypes(t *testing.T) {
	testCases := []struct {
		name        string
		messageType types.MessageType
		content     string
		shouldCache bool
	}{
		{"User", types.MessageTypeUser, "User message", true},
		{"Assistant", types.MessageTypeAssistant, "Assistant message", true},
		{"Reasoning", types.MessageTypeAssistantReasoning, "Thinking...", true},
		{"ShellOutput", types.MessageTypeShellOutput, "$ ls", true},
		{"Error", types.MessageTypeError, "Error occurred", true},
		{"Warning", types.MessageTypeWarning, "Warning!", true},
		{"System", types.MessageTypeSystem, "System message", true},
		{"Separator", types.MessageTypeSeparator, "", true},
		{"Cancelled", types.MessageTypeCancelled, "", true},
		{"Spinner", types.MessageTypeSpinner, "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &types.Message{
				Type:    tc.messageType,
				Content: tc.content,
			}
			mv := New(msg)
			mv.SetSize(80, 0)

			// Render
			view := mv.View()
			require.NotEmpty(t, view)

			if tc.shouldCache {
				assert.NotEmpty(t, mv.cachedView, "Message type %s should be cached", tc.messageType)
			} else {
				assert.Empty(t, mv.cachedView, "Message type %s should not be cached", tc.messageType)
			}
		})
	}
}

func TestMessageCaching_MarkdownComplexContent(t *testing.T) {
	content := `# Heading

This is a paragraph with **bold**, *italic*, and ` + "`code`" + `.

` + "```go\n" + `func main() {
    fmt.Println("Hello")
}
` + "```\n\n" + `- List item 1
- List item 2

[Link](https://example.com)
`

	msg := &types.Message{
		Type:    types.MessageTypeAssistant,
		Content: content,
	}
	mv := New(msg)
	mv.SetSize(80, 0)

	// First render
	view1 := mv.View()
	require.NotEmpty(t, view1)
	assert.NotEmpty(t, mv.cachedView)

	// Subsequent renders should use cache
	for range 10 {
		view := mv.View()
		assert.Equal(t, view1, view, "Cached complex markdown should be consistent")
	}
}

func TestMessageCaching_HeightChangeDoesNotInvalidate(t *testing.T) {
	msg := &types.Message{
		Type:    types.MessageTypeAssistant,
		Content: "Test message",
	}
	mv := New(msg)
	mv.SetSize(80, 10)

	// Initial render
	view1 := mv.View()
	require.NotEmpty(t, view1)
	cachedView := mv.cachedView

	// Change height (should NOT invalidate cache since height doesn't affect rendering)
	mv.SetSize(80, 20)
	assert.NotEmpty(t, mv.cachedView, "Cache should NOT be invalidated on height-only change")
	assert.Equal(t, cachedView, mv.cachedView, "Cached content should remain the same")

	// View should still be the same
	view2 := mv.View()
	assert.Equal(t, view1, view2, "View should be unchanged when only height changes")
}
