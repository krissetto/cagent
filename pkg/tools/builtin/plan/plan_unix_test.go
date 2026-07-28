//go:build unix

package plan

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlanTool_UpdateFromFileRejectsNamedPipe proves a non-regular file (here a
// named pipe with no writer) is rejected without hanging and without being
// read. The pipe is opened with O_NONBLOCK — a plain open would block forever
// waiting for a writer — and then refused when the opened descriptor turns out
// not to be a regular file. The timeout guards against a regression to a
// blocking open.
func TestPlanTool_UpdateFromFileRejectsNamedPipe(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	fifo := filepath.Join(t.TempDir(), "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	ctx := t.Context()
	done := make(chan *struct {
		out     string
		isError bool
	}, 1)
	go func() {
		res, _ := tool.updatePlanFromFile(ctx, UpdatePlanFromFileArgs{Name: "p", Path: fifo})
		done <- &struct {
			out     string
			isError bool
		}{res.Output, res.IsError}
	}()

	select {
	case res := <-done:
		assert.True(t, res.isError)
		assert.Contains(t, res.out, "regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("updatePlanFromFile blocked on a named pipe; the open must not block and the descriptor must be rejected before any read")
	}
}

// TestReadPlanFile_RejectsNamedPipe exercises the reader directly: a FIFO with
// no writer must be opened without blocking and rejected on the descriptor
// check, never read. The timeout guards against a regression to a blocking
// open.
func TestReadPlanFile_RejectsNamedPipe(t *testing.T) {
	t.Parallel()

	fifo := filepath.Join(t.TempDir(), "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	type result struct {
		content string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		content, err := readPlanFile(fifo)
		done <- result{content, err}
	}()

	select {
	case res := <-done:
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "not a regular file")
		assert.Empty(t, res.content)
	case <-time.After(5 * time.Second):
		t.Fatal("readPlanFile blocked on a named pipe; the open must not block and the descriptor must be rejected before any read")
	}
}

// TestReadPlanFile_RejectsDevice proves a device node is rejected on the
// opened descriptor without being read: reading /dev/zero would stream
// unbounded zeroes and surface as a "too large" error at best, so the
// distinct "not a regular file" message shows no read happened.
func TestReadPlanFile_RejectsDevice(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("/dev/zero not available: %v", err)
	}

	content, err := readPlanFile("/dev/zero")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
	assert.Empty(t, content)
}

// TestReadPlanFile_RegularFileWithNonBlockingOpen pins the happy path of the
// hang-safe open on this platform: a regular file opened through
// OpenContentFile (O_NONBLOCK) reads back byte-exact.
func TestReadPlanFile_RegularFileWithNonBlockingOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "content.md")
	require.NoError(t, os.WriteFile(path, []byte("plan body\n"), 0o600))

	content, err := readPlanFile(path)
	require.NoError(t, err)
	assert.Equal(t, "plan body\n", content)
}
