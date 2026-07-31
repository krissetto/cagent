package dialog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func configuredScrollview(dialog *BaseDialog) *scrollview.Model {
	view := dialog.newScrollview()
	view.SetSize(20, 2)
	view.SetContent([]string{"one", "two", "three", "four"}, 4)
	return view
}

func TestBaseDialogFactoryObservesDirectScrollviewUpdates(t *testing.T) {
	var dialog BaseDialog
	view := configuredScrollview(&dialog)
	before := dialog.VisualGeneration()

	handled, _ := view.Update(messages.WheelCoalescedMsg{Delta: 1})
	require.True(t, handled)
	require.Greater(t, dialog.VisualGeneration(), before)

	view.ScrollToBottom()
	before = dialog.VisualGeneration()
	view.Update(messages.WheelCoalescedMsg{Delta: 1})
	require.Equal(t, before, dialog.VisualGeneration(), "boundary no-op changed the dialog generation")
}

func TestBaseDialogFactoryObservesChildrenWithoutCancellation(t *testing.T) {
	var dialog BaseDialog
	first := configuredScrollview(&dialog)
	second := configuredScrollview(&dialog)
	before := dialog.VisualGeneration()

	// Multiple mutations of one child before observation and equal-generation
	// mutations of two children still form one visible dialog change.
	first.ScrollBy(1)
	first.ScrollBy(1)
	second.ScrollBy(1)
	require.Equal(t, before+1, dialog.VisualGeneration())
	require.Equal(t, before+1, dialog.VisualGeneration(), "unchanged children were not stable")

	first.ScrollBy(-1)
	require.Equal(t, before+2, dialog.VisualGeneration())
}

func TestDialogRoutesInputDirectlyToOwnedScrollview(t *testing.T) {
	dialog := NewContextDialog(nil).(*contextDialog)
	dialog.scrollview.SetSize(20, 2)
	dialog.scrollview.SetContent([]string{"one", "two", "three", "four"}, 4)
	before := dialog.VisualGeneration()

	updated, _ := dialog.Update(messages.WheelCoalescedMsg{Delta: 1})
	require.Same(t, dialog, updated)
	require.Positive(t, dialog.scrollview.ScrollOffset())
	require.Greater(t, dialog.VisualGeneration(), before)
}

func TestProductionScrollviewsAreCreatedByBaseDialogFactory(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		contents, err := os.ReadFile(entry.Name())
		require.NoError(t, err)
		if entry.Name() == "base.go" {
			require.Equal(t, 1, strings.Count(string(contents), "scrollview.New("))
			continue
		}
		require.NotContains(t, string(contents), "scrollview.New(", entry.Name())
	}
}
