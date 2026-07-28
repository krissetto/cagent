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

// TestFilesystemStorage_FIFOPlanFileFailsFast proves a <name>.json entry
// that is a FIFO with no writer cannot wedge the storage: Get reports it as
// a typed corrupt plan and List degrades it to a warning while listing the
// healthy plans — both promptly. The open is hang-safe (a plain open of a
// FIFO with no writer blocks forever) and the descriptor is rejected before
// any read; the timeouts guard against a regression to a blocking open.
func TestFilesystemStorage_FIFOPlanFileFailsFast(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)
	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "good", Content: new("ok")})
	require.NoError(t, err)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "wedged.json"), 0o600))

	type getResult struct {
		ok  bool
		err error
	}
	getDone := make(chan getResult, 1)
	go func() {
		_, ok, err := s.Get(t.Context(), "wedged")
		getDone <- getResult{ok: ok, err: err}
	}()
	select {
	case res := <-getDone:
		assert.False(t, res.ok)
		var corrupt *CorruptPlanError
		require.ErrorAs(t, res.err, &corrupt)
		assert.Equal(t, "wedged.json", corrupt.File)
		assert.Contains(t, corrupt.Error(), "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("Get blocked on a FIFO plan file; the open must not block and the descriptor must be rejected before any read")
	}

	type listResult struct {
		plans    []Summary
		warnings []string
		err      error
	}
	listDone := make(chan listResult, 1)
	go func() {
		plans, warnings, err := s.List(t.Context())
		listDone <- listResult{plans: plans, warnings: warnings, err: err}
	}()
	select {
	case res := <-listDone:
		require.NoError(t, res.err)
		require.Len(t, res.plans, 1, "the healthy plan must still be listed")
		assert.Equal(t, "good", res.plans[0].Name)
		require.Len(t, res.warnings, 1, "the FIFO must surface as a warning, not abort the listing")
		assert.Contains(t, res.warnings[0], "wedged")
		assert.Contains(t, res.warnings[0], "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("List blocked on a FIFO plan file; the open must not block and the descriptor must be rejected before any read")
	}
}

// TestFilesystemStorage_UpsertRejectsFIFOPlanFileFast proves the mutation
// path fails fast too: Upsert's pre-read goes through the same hang-safe
// load, so a FIFO squatting on the plan file yields a typed corrupt error
// instead of a wedged write holding the cross-process lock forever.
func TestFilesystemStorage_UpsertRejectsFIFOPlanFileFast(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "wedged.json"), 0o600))

	done := make(chan error, 1)
	go func() {
		_, err := s.Upsert(t.Context(), UpsertRequest{Name: "wedged", Content: new("x")})
		done <- err
	}()
	select {
	case err := <-done:
		var corrupt *CorruptPlanError
		require.ErrorAs(t, err, &corrupt)
	case <-time.After(5 * time.Second):
		t.Fatal("Upsert blocked on a FIFO plan file; the pre-read must not block")
	}
}

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
