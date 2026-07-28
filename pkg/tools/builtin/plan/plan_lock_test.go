package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperEnv gates TestPlanLockHelperProcess so it only runs as a spawned
// subprocess, never as a regular test.
const helperEnv = "PLAN_LOCK_HELPER"

// Helper exit codes, the subprocess's only way to report its outcome.
const (
	helperExitSuccess  = 0
	helperExitConflict = 3
	helperExitFailure  = 4
)

// helperContentSize is how much content each racing helper writes, large
// enough that a torn or interleaved write would be detectable.
const helperContentSize = 256 << 10

func TestStorage_LockSentinelPersistsAndIsNotAPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, lockFileName))

	// The sentinel is invisible to List: no extra plan, no warning.
	plans, warnings, err := s.List(t.Context())
	require.NoError(t, err)
	require.Len(t, plans, 1)
	assert.Equal(t, "p", plans[0].Name)
	assert.Empty(t, warnings)

	// Deleting the last plan leaves the sentinel in place: removing it would
	// let another process lock a different inode and lose mutual exclusion.
	deleted, err := s.Delete(t.Context(), "p", nil)
	require.NoError(t, err)
	assert.True(t, deleted)
	require.FileExists(t, filepath.Join(dir, lockFileName))
}

func TestStorage_WriteHonorsContextWhileLockHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)

	release, err := acquireFileLock(t.Context(), filepath.Join(dir, lockFileName))
	require.NoError(t, err)
	defer release()

	// A write blocked on the lock gives up when its context expires...
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = s.Upsert(ctx, UpsertRequest{Name: "p", Content: new("v2")})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// ...and so does a delete.
	ctx2, cancel2 := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel2()
	_, err = s.Delete(ctx2, "p", new(1))
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// The plan is untouched by the abandoned operations.
	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v1", got.Content)
	assert.Equal(t, 1, got.Revision)

	// An already-cancelled context fails fast without touching the lock.
	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	_, err = s.Upsert(cancelled, UpsertRequest{Name: "p", Content: new("v2")})
	require.ErrorIs(t, err, context.Canceled)
}

// TestStorage_ReadsStayResponsiveWhileWriterWaitsForLock guards the lock
// ordering fix: mutations acquire the cross-process sentinel before the
// in-process mutex and hold the mutex only for their local window, so a
// writer parked on a sentinel held elsewhere (here: held externally, playing
// the role of another process) must never stall Get or List on the same
// storage.
func TestStorage_ReadsStayResponsiveWhileWriterWaitsForLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)
	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "q", Content: new("w1")})
	require.NoError(t, err)

	release, err := acquireFileLock(t.Context(), filepath.Join(dir, lockFileName))
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	// One Upsert and one Delete block on the held sentinel. Both goroutines
	// are joined below, so the test also proves neither leaks.
	upsertDone := make(chan error, 1)
	go func() {
		_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v2")})
		upsertDone <- err
	}()
	deleteDone := make(chan error, 1)
	go func() {
		deleted, err := s.Delete(t.Context(), "q", new(1))
		if err == nil && !deleted {
			err = errors.New("delete reported nothing to delete")
		}
		deleteDone <- err
	}()

	// Give both writers time to reach the sentinel wait; neither may finish.
	select {
	case err := <-upsertDone:
		t.Fatalf("upsert completed while the sentinel was held externally (err=%v)", err)
	case err := <-deleteDone:
		t.Fatalf("delete completed while the sentinel was held externally (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Get and List must complete with the old, complete revisions while the
	// writers stay blocked. They run in a goroutine so a regression (reader
	// queued behind the blocked writer) fails the test instead of hanging it;
	// the goroutine uses assert, never require, as FailNow must not be called
	// off the test goroutine.
	readsDone := make(chan struct{})
	go func() {
		defer close(readsDone)
		got, ok, err := s.Get(t.Context(), "p")
		assert.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, 1, got.Revision)
		assert.Equal(t, "v1", got.Content)

		plans, warnings, err := s.List(t.Context())
		assert.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Len(t, plans, 2)
	}()
	select {
	case <-readsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Get/List blocked behind a writer waiting for the cross-process lock")
	}

	// The reads finished while both writers were still parked on the sentinel.
	select {
	case err := <-upsertDone:
		t.Fatalf("upsert completed while the sentinel was held externally (err=%v)", err)
	case err := <-deleteDone:
		t.Fatalf("delete completed while the sentinel was held externally (err=%v)", err)
	default:
	}

	release()
	released = true

	for name, done := range map[string]chan error{"upsert": upsertDone, "delete": deleteDone} {
		select {
		case err := <-done:
			require.NoError(t, err, "%s failed after the lock was released", name)
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not complete after the lock was released", name)
		}
	}

	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.Revision)
	assert.Equal(t, "v2", got.Content)

	_, ok, err = s.Get(t.Context(), "q")
	require.NoError(t, err)
	assert.False(t, ok, "q must be deleted once the blocked delete went through")
}

// TestStorage_BlockedWritersCancelWithoutLeak proves mutations parked on the
// sentinel are abandoned promptly when their context is cancelled — each
// goroutine exits with context.Canceled instead of leaking — and that the
// plan is untouched and still readable afterwards, sentinel still held.
func TestStorage_BlockedWritersCancelWithoutLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)

	release, err := acquireFileLock(t.Context(), filepath.Join(dir, lockFileName))
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	upsertDone := make(chan error, 1)
	go func() {
		_, err := s.Upsert(ctx, UpsertRequest{Name: "p", Content: new("v2")})
		upsertDone <- err
	}()
	deleteDone := make(chan error, 1)
	go func() {
		_, err := s.Delete(ctx, "p", new(1))
		deleteDone <- err
	}()

	select {
	case err := <-upsertDone:
		t.Fatalf("upsert completed while the sentinel was held externally (err=%v)", err)
	case err := <-deleteDone:
		t.Fatalf("delete completed while the sentinel was held externally (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	for name, done := range map[string]chan error{"upsert": upsertDone, "delete": deleteDone} {
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled, "%s must give up on cancellation", name)
		case <-time.After(10 * time.Second):
			t.Fatalf("cancelled %s did not return; writer goroutine leaked", name)
		}
	}

	// The abandoned mutations changed nothing, and reads still work with the
	// sentinel held.
	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.Revision)
	assert.Equal(t, "v1", got.Content)
}

// TestStorage_LockBlocksGuardedWriteAcrossProcesses proves the cross-process
// guarantee directly: while this process holds the filesystem lock, a second
// docker-agent process cannot enter its guarded write; once released, the
// write goes through.
func TestStorage_LockBlocksGuardedWriteAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)

	release, err := acquireFileLock(t.Context(), filepath.Join(dir, lockFileName))
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	helper := startLockHelper(t, dir, "p", 1, "A", 16, filepath.Join(t.TempDir(), "go"))
	done := make(chan error, 1)
	go func() { done <- helper.cmd.Wait() }()

	// The ready file flags that the helper is past setup; the go signal then
	// sends it into Upsert, so from here on "has not exited" means "is blocked
	// on the lock".
	waitForFile(t, helper.readyFile)
	require.NoError(t, os.WriteFile(helper.goFile, nil, 0o600))

	select {
	case err := <-done:
		t.Fatalf("helper completed a guarded write while the parent held the lock (err=%v, stderr=%s)", err, helper.stderr)
	case <-time.After(300 * time.Millisecond):
	}

	// The blocked writer changed nothing.
	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.Revision)
	assert.Equal(t, "v1", got.Content)

	release()
	released = true

	select {
	case err := <-done:
		require.NoError(t, err, "helper stderr: %s", helper.stderr)
	case <-time.After(10 * time.Second):
		_ = helper.cmd.Process.Kill()
		t.Fatalf("helper did not acquire the lock after the parent released it (stderr: %s)", helper.stderr)
	}

	got, ok, err = s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.Revision)
	assert.Equal(t, strings.Repeat("A", 16), got.Content)
}

// TestStorage_CrossProcessRaceOneWinnerOneConflict spawns two separate
// processes that both try to write the same plan with the same expected
// revision. A shared go-file barrier releases both into Upsert at the same
// moment — without depending on the lock under test — so their
// read-modify-write windows overlap: exactly one must win, the other must get
// a version conflict, the winner's content must be complete, and the revision
// must advance only once.
func TestStorage_CrossProcessRaceOneWinnerOneConflict(t *testing.T) {
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)

	goFile := filepath.Join(t.TempDir(), "go")
	helperA := startLockHelper(t, dir, "p", 1, "a", helperContentSize, goFile)
	helperB := startLockHelper(t, dir, "p", 1, "b", helperContentSize, goFile)

	// Both helpers are lined up spinning on the go file; releasing it starts
	// both guarded writes within a few hundred microseconds of each other.
	waitForFile(t, helperA.readyFile)
	waitForFile(t, helperB.readyFile)
	require.NoError(t, os.WriteFile(goFile, nil, 0o600))

	exitA := waitHelperExit(t, helperA)
	exitB := waitHelperExit(t, helperB)

	require.ElementsMatch(t, []int{helperExitSuccess, helperExitConflict}, []int{exitA, exitB},
		"exactly one racer must win and one must get a version conflict (A=%d stderr=%s; B=%d stderr=%s)",
		exitA, helperA.stderr, exitB, helperB.stderr)

	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.Revision, "the revision must advance exactly once")

	winner := "a"
	if exitB == helperExitSuccess {
		winner = "b"
	}
	assert.Equal(t, strings.Repeat(winner, helperContentSize), got.Content,
		"the winner's content must be preserved in full")
}

// TestPlanLockHelperProcess is not a real test: it is the subprocess body for
// the cross-process tests above, gated on PLAN_LOCK_HELPER. It touches its
// ready file, waits for the go signal, performs one guarded write, and
// reports the outcome through its exit code: 0 on success, 3 on a version
// conflict, 4 on any other failure.
func TestPlanLockHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) != 7 {
		fmt.Fprintf(os.Stderr, "helper: want 7 args after --, got %v\n", args)
		os.Exit(helperExitFailure)
	}
	dir, name, revStr, marker, sizeStr, readyFile, goFile := args[0], args[1], args[2], args[3], args[4], args[5], args[6]
	rev, err := strconv.Atoi(revStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: bad revision %q: %v\n", revStr, err)
		os.Exit(helperExitFailure)
	}
	size, err := strconv.Atoi(sizeStr)
	if err != nil || size <= 0 {
		fmt.Fprintf(os.Stderr, "helper: bad size %q: %v\n", sizeStr, err)
		os.Exit(helperExitFailure)
	}

	content := strings.Repeat(marker, size)
	if err := os.WriteFile(readyFile, nil, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "helper: writing ready file: %v\n", err)
		os.Exit(helperExitFailure)
	}

	// Spin on the go signal with a tight interval so sibling helpers enter
	// Upsert as close to simultaneously as possible.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(goFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "helper: timed out waiting for go signal")
			os.Exit(helperExitFailure)
		}
		time.Sleep(200 * time.Microsecond) //nolint:forbidigo // cross-process file barrier: no in-process primitive to synchronize two processes on
	}

	_, err = NewFilesystemStorage(dir).Upsert(t.Context(), UpsertRequest{
		Name:             name,
		Content:          &content,
		ExpectedRevision: &rev,
	})
	var conflict *VersionConflictError
	switch {
	case err == nil:
		os.Exit(helperExitSuccess)
	case errors.As(err, &conflict):
		os.Exit(helperExitConflict)
	default:
		fmt.Fprintf(os.Stderr, "helper: upsert: %v\n", err)
		os.Exit(helperExitFailure)
	}
}

// lockHelper tracks one spawned helper process and its diagnostics.
type lockHelper struct {
	cmd       *exec.Cmd
	stderr    *bytes.Buffer
	readyFile string
	goFile    string
}

// startLockHelper re-executes the test binary as a separate process that
// performs one guarded write of size bytes of marker on the named plan. The
// helper reports readiness through its ready file and then waits for goFile
// to exist before writing, so the caller controls when the write starts.
func startLockHelper(t *testing.T, dir, name string, expectedRevision int, marker string, size int, goFile string) *lockHelper {
	t.Helper()
	readyFile := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=TestPlanLockHelperProcess", "--",
		dir, name, strconv.Itoa(expectedRevision), marker, strconv.Itoa(size), readyFile, goFile)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	require.NoError(t, cmd.Start())
	return &lockHelper{cmd: cmd, stderr: stderr, readyFile: readyFile, goFile: goFile}
}

// waitHelperExit waits for the helper to finish and returns its exit code.
func waitHelperExit(t *testing.T, h *lockHelper) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return helperExitSuccess
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			return exit.ExitCode()
		}
		t.Fatalf("helper did not exit cleanly: %v (stderr: %s)", err, h.stderr)
	case <-time.After(10 * time.Second):
		_ = h.cmd.Process.Kill()
		t.Fatalf("helper did not finish in time (stderr: %s)", h.stderr)
	}
	return -1
}

// waitForFile polls until path exists, failing the test after a deadline.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 10*time.Second, 5*time.Millisecond, "timed out waiting for %s", path)
}

// --- Bounded decoding --------------------------------------------------------

// TestStorage_OversizedPlanFileReportedCorrupt proves a stored file beyond
// maxEncodedPlanSize is never slurped into memory whole: it surfaces as a
// typed corrupt-plan error on Get, a warning on List, blocks guarded writes,
// and is recoverable with an unguarded delete — exactly like undecodable
// JSON.
func TestStorage_OversizedPlanFileReportedCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.json"), make([]byte, maxEncodedPlanSize+1), 0o600))

	_, _, err := s.Get(t.Context(), "big")
	var corrupt *CorruptPlanError
	require.ErrorAs(t, err, &corrupt)
	assert.Equal(t, "big.json", corrupt.File)
	assert.Contains(t, err.Error(), "exceeds")

	plans, warnings, err := s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plans)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "big")

	// A write cannot silently replace it: the pre-write load fails.
	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "big", Content: new("x")})
	require.ErrorAs(t, err, &corrupt)

	deleted, err := s.Delete(t.Context(), "big", nil)
	require.NoError(t, err)
	assert.True(t, deleted)
}

// TestStorage_ContentAtSizeCapIsStored proves the advertised content cap is
// exact: content of exactly MaxPlanContentSize — including bytes JSON must
// escape — is persisted and read back intact, because the stored-file bound
// accounts for worst-case escaping instead of capping the encoded form at the
// content limit.
func TestStorage_ContentAtSizeCapIsStored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	// A quarter of escapable bytes (" expands to 2, \n to 2) proves escaping
	// headroom without paying for the 6x worst case in test time.
	content := strings.Repeat(`ab"`+"\n", MaxPlanContentSize/4)
	require.Len(t, content, MaxPlanContentSize)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "big", Content: &content})
	require.NoError(t, err, "content of exactly the advertised cap must be accepted")

	got, ok, err := s.Get(t.Context(), "big")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, content, got.Content)
}

// TestStorage_WorstCaseEscapedContentAtSizeCapIsStored exercises the exact
// worst case maxEncodedPlanSize is derived from: NUL bytes escape as \u0000
// (6 bytes each, the maximum JSON expansion), so content of exactly
// MaxPlanContentSize made entirely of them encodes to the full
// 6*MaxPlanContentSize — and must still be persisted and read back intact.
func TestStorage_WorstCaseEscapedContentAtSizeCapIsStored(t *testing.T) {
	t.Parallel()

	// Pin the 6x expansion formula the bound relies on before paying for the
	// full-size write: every NUL encodes to exactly 6 bytes (plus quotes).
	encoded, err := json.Marshal(strings.Repeat("\x00", 1024))
	require.NoError(t, err)
	require.Len(t, encoded, 6*1024+2, "a control byte must escape to exactly 6 bytes")

	dir := t.TempDir()
	s := NewFilesystemStorage(dir)
	content := strings.Repeat("\x00", MaxPlanContentSize)

	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "worst", Content: &content})
	require.NoError(t, err, "worst-case escaped content of exactly the cap must be accepted")

	info, err := os.Stat(filepath.Join(dir, "worst.json"))
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(6*MaxPlanContentSize), "the stored file must carry the 6x-escaped body")
	assert.LessOrEqual(t, info.Size(), int64(maxEncodedPlanSize), "the stored file must fit the bound load applies")

	got, ok, err := s.Get(t.Context(), "worst")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, content, got.Content)
}

// TestStorage_LargeMetadataBackwardCompatible proves free-form metadata has
// no per-field cap: a plan stored by an earlier build with labels far beyond
// any "reasonable" size keeps loading, survives a content-only update with
// its metadata preserved, and new writes may carry equally large labels.
func TestStorage_LargeMetadataBackwardCompatible(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	// Plant a legacy plan file directly, as a previous build (whole-file
	// bound only, no metadata cap) could have written it: every metadata
	// field well past 4 KiB.
	bigStatus := strings.Repeat("s", 8<<10)
	bigTitle := strings.Repeat("t", 8<<10)
	bigAuthor := strings.Repeat("a", 8<<10)
	legacy, err := json.Marshal(Plan{
		Name:      "legacy",
		Title:     bigTitle,
		Author:    bigAuthor,
		Status:    bigStatus,
		Content:   "legacy content",
		Revision:  3,
		UpdatedAt: "2024-01-02T03:04:05Z",
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "legacy.json"), legacy, 0o600))

	got, ok, err := s.Get(t.Context(), "legacy")
	require.NoError(t, err, "a legacy plan with large metadata must keep loading")
	require.True(t, ok)
	assert.Equal(t, bigStatus, got.Status)

	// A content-only update must succeed and preserve the large metadata.
	updated, err := s.Upsert(t.Context(), UpsertRequest{Name: "legacy", Content: new("new content")})
	require.NoError(t, err, "updating only the content of a legacy plan must not trip any metadata cap")
	assert.Equal(t, "new content", updated.Content)
	assert.Equal(t, bigTitle, updated.Title)
	assert.Equal(t, bigAuthor, updated.Author)
	assert.Equal(t, bigStatus, updated.Status)
	assert.Equal(t, 4, updated.Revision)

	// New writes may set equally large labels.
	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "fresh", Content: new("body"), Status: &bigStatus})
	require.NoError(t, err)
	got, ok, err = s.Get(t.Context(), "fresh")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, bigStatus, got.Status)
}

// TestStorage_SaveRejectsPlanOverSizeCap proves the write side of the cap: a
// plan whose content exceeds the advertised bound is rejected up front, so
// the storage never persists a file it would then refuse to decode.
func TestStorage_SaveRejectsPlanOverSizeCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("v1")})
	require.NoError(t, err)

	huge := strings.Repeat("x", MaxPlanContentSize+1)
	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: &huge})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content is too large")

	// The rejected write left the previous revision fully intact.
	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v1", got.Content)
	assert.Equal(t, 1, got.Revision)
}
