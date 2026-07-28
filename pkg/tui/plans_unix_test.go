//go:build !windows

package tui

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
)

// TestHandlePlanEditorClosed_NoInlineDraftRead proves Update never reads the
// draft on the event loop: the draft path is a FIFO with no writer, on which
// an inline read (a plain open without O_NONBLOCK) would block forever and
// hang this test. Update must return a command immediately; the command then
// opens the FIFO hang-safe, rejects it on the descriptor as non-regular, and
// leaves the path in place.
func TestHandlePlanEditorClosed_NoInlineDraftRead(t *testing.T) {
	t.Parallel()
	m, svc, _, _ := newPlansTestModel(t)

	fifo := filepath.Join(t.TempDir(), "draft.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a FIFO on this system: %v", err)
	}

	// A regression to an inline draft read would block this call forever
	// and fail the test by timeout.
	_, cmd := m.Update(planEditorClosedMsg{ref: plans.SharedRef("fresh"), create: true, path: fifo})
	require.NotNil(t, cmd, "Update must defer the draft read to a command")

	result := cmd()
	writeResult, ok := result.(planWriteResultMsg)
	require.True(t, ok, "the command must yield a typed result, got %T", result)
	require.Error(t, writeResult.readErr)
	assert.Contains(t, writeResult.readErr.Error(), "not a regular file")

	_, notifyCmd := m.Update(result)
	texts := notificationTexts(collectMsgs(notifyCmd))
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "not a regular file")

	_, err := svc.Get(t.Context(), plans.SharedRef("fresh"))
	require.Error(t, err, "nothing may be written from an unreadable draft")
	_, err = os.Stat(fifo)
	require.NoError(t, err, "the draft path must be preserved on a read failure")
}
