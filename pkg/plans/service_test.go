package plans

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
)

// newTestService builds a Service over a fresh filesystem-backed shared
// storage and an isolated session-plans directory, returning both directories
// for tests that plant files directly.
func newTestService(t *testing.T) (svc Service, sharedDir, sessionDir string) {
	t.Helper()
	sharedDir = t.TempDir()
	sessionDir = t.TempDir()
	svc = NewService(plan.NewFilesystemStorage(sharedDir), WithSessionDir(sessionDir))
	return svc, sharedDir, sessionDir
}

func mustCreate(t *testing.T, svc Service, name, content string) Plan {
	t.Helper()
	p, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef(name), Content: content})
	require.NoError(t, err)
	return p
}

func writeSessionPlan(t *testing.T, dir, sessionID, content string) string {
	t.Helper()
	path, err := sessionplan.WriteContent(dir, sessionID, content)
	require.NoError(t, err)
	return path
}

// --- List --------------------------------------------------------------------

func TestService_ListEmpty(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	result, err := svc.List(t.Context(), ListOptions{})
	require.NoError(t, err)
	assert.NotNil(t, result.Plans)
	assert.Empty(t, result.Plans)
	assert.Empty(t, result.Warnings)
}

func TestService_ListEmptyWithSessionID(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	// A missing session plan is not an error during List.
	result, err := svc.List(t.Context(), ListOptions{SessionID: "sess-1"})
	require.NoError(t, err)
	assert.Empty(t, result.Plans)
	assert.Empty(t, result.Warnings)
}

func TestService_ListSharedMetadataOnly(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("beta"), Content: "b", Status: "draft"})
	require.NoError(t, err)
	_, err = svc.Create(t.Context(), CreateRequest{Ref: SharedRef("alpha"), Content: "a", Title: "Alpha", Author: "alice"})
	require.NoError(t, err)

	result, err := svc.List(t.Context(), ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Plans, 2)

	// Sorted by name, metadata carried through, content omitted.
	first := result.Plans[0]
	assert.Equal(t, ScopeShared, first.Scope)
	assert.Equal(t, "alpha", first.Name)
	assert.Equal(t, "Alpha", first.Title)
	assert.Equal(t, "alice", first.Author)
	assert.Empty(t, first.Content)
	require.NotNil(t, first.Version)
	assert.Equal(t, 1, *first.Version)
	assert.False(t, first.UpdatedAt.IsZero())
	assert.WithinDuration(t, time.Now(), first.UpdatedAt, time.Minute)

	assert.Equal(t, "beta", result.Plans[1].Name)
	assert.Equal(t, "draft", result.Plans[1].Status)
}

func TestService_ListIncludesCurrentSessionPlanFirst(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newTestService(t)
	mustCreate(t, svc, "alpha", "a")
	path := writeSessionPlan(t, sessionDir, "sess-1", "# session plan")

	result, err := svc.List(t.Context(), ListOptions{SessionID: "sess-1"})
	require.NoError(t, err)
	require.Len(t, result.Plans, 2)

	sess := result.Plans[0]
	assert.Equal(t, ScopeSession, sess.Scope)
	assert.Equal(t, "sess-1", sess.Name)
	assert.Equal(t, "sess-1", sess.SessionID)
	assert.Equal(t, path, sess.Path)
	assert.Nil(t, sess.Version, "session plans have no version")
	assert.Empty(t, sess.Content, "List is metadata only")

	// The timestamp comes from file metadata.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, sess.UpdatedAt.Equal(info.ModTime().UTC()))

	assert.Equal(t, ScopeShared, result.Plans[1].Scope)
	assert.Equal(t, "alpha", result.Plans[1].Name)
}

func TestService_ListConsultsOnlySuppliedSession(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newTestService(t)
	writeSessionPlan(t, sessionDir, "current", "mine")
	writeSessionPlan(t, sessionDir, "stale-other", "left behind")

	result, err := svc.List(t.Context(), ListOptions{SessionID: "current"})
	require.NoError(t, err)
	require.Len(t, result.Plans, 1)
	assert.Equal(t, "current", result.Plans[0].Name)
}

func TestService_ListInvalidSessionID(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	_, err := svc.List(t.Context(), ListOptions{SessionID: "../escape"})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
}

func TestService_ListWarnsOnCorruptShared(t *testing.T) {
	t.Parallel()
	svc, sharedDir, _ := newTestService(t)
	mustCreate(t, svc, "good", "ok")
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "bad.json"), []byte("{nope"), 0o600))

	result, err := svc.List(t.Context(), ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Plans, 1)
	assert.Equal(t, "good", result.Plans[0].Name)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "bad")
}

func TestService_ListStorageFailure(t *testing.T) {
	t.Parallel()
	base := errors.New("backend boom")
	svc := NewService(failingStorage{err: base}, WithSessionDir(t.TempDir()))

	_, err := svc.List(t.Context(), ListOptions{})
	var storageErr *StorageError
	require.ErrorAs(t, err, &storageErr)
	assert.Equal(t, ScopeShared, storageErr.Scope)
	assert.Equal(t, "list", storageErr.Op)
	assert.ErrorIs(t, err, base)
}

// --- Get ---------------------------------------------------------------------

func TestService_GetShared(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	_, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("release"), Content: "the body", Title: "Release", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)

	p, err := svc.Get(t.Context(), SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, ScopeShared, p.Scope)
	assert.Equal(t, "release", p.Name)
	assert.Equal(t, "the body", p.Content)
	assert.Equal(t, "Release", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
	require.NotNil(t, p.Version)
	assert.Equal(t, 1, *p.Version)
	assert.False(t, p.UpdatedAt.IsZero())
	assert.Empty(t, p.SessionID)
	assert.Empty(t, p.Path)
}

func TestService_GetSharedNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	_, err := svc.Get(t.Context(), SharedRef("missing"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
	assert.Equal(t, ScopeShared, notFound.Scope)
	assert.Equal(t, "missing", notFound.Name)
}

func TestService_GetSharedInvalidName(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	for _, name := range []string{"", "UPPER", "../escape", "a/b", "has space"} {
		_, err := svc.Get(t.Context(), SharedRef(name))
		var invalid *ValidationError
		require.ErrorAs(t, err, &invalid, "name %q should be invalid", name)
		assert.Contains(t, invalid.Message, "invalid plan name")
	}
}

func TestService_GetSharedCorrupt(t *testing.T) {
	t.Parallel()
	svc, sharedDir, _ := newTestService(t)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "broken.json"), []byte("{not json"), 0o600))

	_, err := svc.Get(t.Context(), SharedRef("broken"))
	var corrupt *CorruptError
	require.ErrorAs(t, err, &corrupt)
	assert.Equal(t, ScopeShared, corrupt.Scope)
	assert.Equal(t, "broken", corrupt.Name)
	// The storage's typed cause is preserved through the wrap.
	var cause *plan.CorruptPlanError
	require.ErrorAs(t, err, &cause)

	var notFound *NotFoundError
	require.NotErrorAs(t, err, &notFound, "a corrupt plan must not read as missing")
}

func TestService_GetSession(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newTestService(t)
	path := writeSessionPlan(t, sessionDir, "sess-1", "# my plan\nstep 1\n")

	p, err := svc.Get(t.Context(), SessionRef("sess-1"))
	require.NoError(t, err)
	assert.Equal(t, ScopeSession, p.Scope)
	assert.Equal(t, "sess-1", p.Name)
	assert.Equal(t, "sess-1", p.SessionID)
	assert.Equal(t, "# my plan\nstep 1\n", p.Content)
	assert.Equal(t, path, p.Path)
	assert.Nil(t, p.Version, "session plans must not expose a version")
	assert.Empty(t, p.Status)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, p.UpdatedAt.Equal(info.ModTime().UTC()))
}

func TestService_GetSessionNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	// Missing is not-found during Get, unlike List where it is skipped.
	_, err := svc.Get(t.Context(), SessionRef("ghost"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
	assert.Equal(t, ScopeSession, notFound.Scope)
	assert.Equal(t, "ghost", notFound.Name)
}

func TestService_GetSessionInvalidID(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	for _, id := range []string{"", "../escape", "a/b"} {
		_, err := svc.Get(t.Context(), SessionRef(id))
		var invalid *ValidationError
		require.ErrorAs(t, err, &invalid, "session ID %q should be invalid", id)
	}
}

func TestService_GetUnknownScope(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	_, err := svc.Get(t.Context(), Ref{Scope: "bogus", Name: "p"})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "scope")
}

// --- Create ------------------------------------------------------------------

func TestService_Create(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	p, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("release"), Content: "v1", Title: "T", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)
	assert.Equal(t, ScopeShared, p.Scope)
	assert.Equal(t, "release", p.Name)
	assert.Equal(t, "v1", p.Content)
	assert.Equal(t, "T", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
	require.NotNil(t, p.Version)
	assert.Equal(t, 1, *p.Version)
}

func TestService_CreateIsCreateOnly(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "original")

	// A second create conflicts instead of overwriting, exposing the
	// current version so the frontend can switch to an update.
	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("p"), Content: "clobber"})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "p", conflict.Name)
	assert.Equal(t, 0, conflict.Expected)
	assert.Equal(t, 1, conflict.Current)

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "original", got.Content)
}

func TestService_CreateValidation(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("p"), Content: ""})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "content must not be empty")

	for _, name := range []string{"", "UPPER", "../escape"} {
		_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef(name), Content: "x"})
		require.ErrorAs(t, err, &invalid, "name %q should be invalid", name)
		assert.Contains(t, invalid.Message, "invalid plan name")
	}
}

// --- Update ------------------------------------------------------------------

func TestService_UpdatePreservesOmittedMetadata(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	_, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("p"), Content: "v1", Title: "Original", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)

	p, err := svc.Update(t.Context(), UpdateRequest{
		Ref: SharedRef("p"), Content: "v2", Author: new("bob"), ExpectedVersion: new(1),
	})
	require.NoError(t, err)
	assert.Equal(t, "v2", p.Content)
	assert.Equal(t, "Original", p.Title, "nil title pointer preserves the previous value")
	assert.Equal(t, "bob", p.Author)
	assert.Equal(t, "draft", p.Status)
	require.NotNil(t, p.Version)
	assert.Equal(t, 2, *p.Version)

	// An explicit empty string overwrites rather than preserves.
	p, err = svc.Update(t.Context(), UpdateRequest{
		Ref: SharedRef("p"), Content: "v3", Title: new(""), ExpectedVersion: new(2),
	})
	require.NoError(t, err)
	assert.Empty(t, p.Title)
}

func TestService_UpdateStaleConflictPreservesNewerContent(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "stale", ExpectedVersion: new(1)})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 1, conflict.Expected)
	assert.Equal(t, 2, conflict.Current, "the conflict must expose the current version")

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Content, "the losing write must not touch the plan")
	assert.Equal(t, 2, *got.Version)
}

func TestService_UpdateForceReplacesUnconditionally(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	// nil expected version is the deliberate force policy.
	p, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "forced"})
	require.NoError(t, err)
	assert.Equal(t, "forced", p.Content)
	assert.Equal(t, 3, *p.Version)
}

func TestService_UpdateNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	var notFound *NotFoundError

	// Update never creates, not even when forced.
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("ghost"), Content: "x"})
	require.ErrorAs(t, err, &notFound)

	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("ghost"), Content: "x", ExpectedVersion: new(1)})
	require.ErrorAs(t, err, &notFound)

	_, err = svc.Get(t.Context(), SharedRef("ghost"))
	require.ErrorAs(t, err, &notFound, "the failed update must not have created the plan")
}

func TestService_UpdateEmptyContent(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")

	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: ""})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "content must not be empty")
}

// --- SetStatus ---------------------------------------------------------------

func TestService_SetStatusFreeForm(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	// Status is free-form: any non-empty string is accepted.
	for i, status := range []string{"in-progress", "🚀 shipping", "waiting on review/QA"} {
		p, err := svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: status, ExpectedVersion: new(i + 1)})
		require.NoError(t, err)
		assert.Equal(t, status, p.Status)
		assert.Equal(t, i+2, *p.Version, "setting the status is a write and bumps the version")
		assert.Equal(t, "body", p.Content, "the body is preserved")
	}
}

func TestService_SetStatusStaleConflict(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("p"), Content: "body", Status: "draft"})
	require.NoError(t, err)
	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, err = svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: "done", ExpectedVersion: new(1)})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 2, conflict.Current)

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "draft", got.Status, "the losing status write must not touch the plan")
}

func TestService_SetStatusValidation(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	_, err := svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: ""})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "status must not be empty")
}

func TestService_SetStatusNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	_, err := svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("ghost"), Status: "done"})
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

// --- Delete ------------------------------------------------------------------

func TestService_DeleteWithMatchingVersion(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	require.NoError(t, svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("p"), ExpectedVersion: new(1)}))

	_, err := svc.Get(t.Context(), SharedRef("p"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestService_DeleteStaleConflictPreservesPlan(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	err = svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("p"), ExpectedVersion: new(1)})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 2, conflict.Current)

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Content, "the conflicting delete must leave the plan in place")
}

func TestService_DeleteForce(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")

	// nil expected version deletes unconditionally.
	require.NoError(t, svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("p")}))
}

func TestService_DeleteNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)

	err := svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("ghost")})
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestService_DeleteCorrupt(t *testing.T) {
	t.Parallel()
	svc, sharedDir, _ := newTestService(t)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "broken.json"), []byte("{nope"), 0o600))

	// A guarded delete cannot verify the revision of a corrupt plan.
	err := svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("broken"), ExpectedVersion: new(1)})
	var corrupt *CorruptError
	require.ErrorAs(t, err, &corrupt)

	// A force delete still recovers it.
	require.NoError(t, svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("broken")}))
	assert.NoFileExists(t, filepath.Join(sharedDir, "broken.json"))
}

// --- Session mutations are unsupported ----------------------------------------

func TestService_SessionMutationsUnsupported(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newTestService(t)
	writeSessionPlan(t, sessionDir, "sess-1", "# plan")
	ref := SessionRef("sess-1")
	ctx := t.Context()

	calls := map[string]func() error{
		"create": func() error {
			_, err := svc.Create(ctx, CreateRequest{Ref: ref, Content: "x"})
			return err
		},
		"update": func() error {
			_, err := svc.Update(ctx, UpdateRequest{Ref: ref, Content: "x"})
			return err
		},
		"set_status": func() error {
			_, err := svc.SetStatus(ctx, SetStatusRequest{Ref: ref, Status: "done"})
			return err
		},
		"delete": func() error {
			return svc.Delete(ctx, DeleteRequest{Ref: ref})
		},
	}

	for op, call := range calls {
		err := call()
		var unsupported *UnsupportedError
		require.ErrorAs(t, err, &unsupported, "op %s", op)
		assert.Equal(t, ScopeSession, unsupported.Scope)
		assert.Equal(t, op, unsupported.Op)
		assert.NotEmpty(t, unsupported.Reason, "the error must tell the caller what to do instead")
	}

	// The session plan is untouched by the refused mutations.
	p, err := svc.Get(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, "# plan", p.Content)
}

// --- Export ------------------------------------------------------------------

func TestService_ExportShared(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "the body")

	dest := filepath.Join(t.TempDir(), "nested", "export.md")
	result, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: dest})
	require.NoError(t, err)
	assert.Equal(t, ScopeShared, result.Scope)
	assert.Equal(t, "p", result.Name)
	assert.Equal(t, dest, result.Path)
	require.NotNil(t, result.Version)
	assert.Equal(t, 1, *result.Version)
	assert.Equal(t, len("the body"), result.BytesWritten)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "the body", string(data))
}

func TestService_ExportSession(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newTestService(t)
	writeSessionPlan(t, sessionDir, "sess-1", "# session plan")

	dest := filepath.Join(t.TempDir(), "export.md")
	result, err := svc.Export(t.Context(), ExportRequest{Ref: SessionRef("sess-1"), Path: dest})
	require.NoError(t, err)
	assert.Equal(t, ScopeSession, result.Scope)
	assert.Equal(t, "sess-1", result.Name)
	assert.Nil(t, result.Version, "session plans have no version to export")
	assert.Equal(t, len("# session plan"), result.BytesWritten)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "# session plan", string(data))
}

func TestService_ExportNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	var notFound *NotFoundError

	dest := filepath.Join(t.TempDir(), "export.md")
	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("ghost"), Path: dest})
	require.ErrorAs(t, err, &notFound)
	assert.NoFileExists(t, dest)

	_, err = svc.Export(t.Context(), ExportRequest{Ref: SessionRef("ghost"), Path: dest})
	require.ErrorAs(t, err, &notFound)
	assert.NoFileExists(t, dest)
}

func TestService_ExportValidation(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")
	var invalid *ValidationError

	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: ""})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "path must not be empty")

	_, err = svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: t.TempDir()})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "directory")
}

// --- Storage failures and injection -------------------------------------------

func TestService_StorageFailuresAreTyped(t *testing.T) {
	t.Parallel()
	base := errors.New("backend boom")
	svc := NewService(failingStorage{err: base}, WithSessionDir(t.TempDir()))
	ctx := t.Context()

	calls := map[string]func() error{
		"get": func() error {
			_, err := svc.Get(ctx, SharedRef("p"))
			return err
		},
		"create": func() error {
			_, err := svc.Create(ctx, CreateRequest{Ref: SharedRef("p"), Content: "x"})
			return err
		},
		"update": func() error {
			_, err := svc.Update(ctx, UpdateRequest{Ref: SharedRef("p"), Content: "x"})
			return err
		},
		"set_status": func() error {
			_, err := svc.SetStatus(ctx, SetStatusRequest{Ref: SharedRef("p"), Status: "done"})
			return err
		},
		"delete": func() error {
			return svc.Delete(ctx, DeleteRequest{Ref: SharedRef("p")})
		},
		"export": func() error {
			_, err := svc.Export(ctx, ExportRequest{Ref: SharedRef("p"), Path: filepath.Join(t.TempDir(), "x.md")})
			return err
		},
	}

	for op, call := range calls {
		err := call()
		var storageErr *StorageError
		require.ErrorAs(t, err, &storageErr, "op %s", op)
		assert.Equal(t, ScopeShared, storageErr.Scope, "op %s", op)
		assert.ErrorIs(t, err, base, "op %s must preserve the cause", op)
	}
}

// TestService_InjectedInMemoryStorage proves the service works against any
// plan.Storage, not just the filesystem default.
func TestService_InjectedInMemoryStorage(t *testing.T) {
	t.Parallel()
	svc := NewService(newMemStorage(), WithSessionDir(t.TempDir()))
	ctx := t.Context()

	p, err := svc.Create(ctx, CreateRequest{Ref: SharedRef("p"), Content: "v1", Status: "draft"})
	require.NoError(t, err)
	assert.Equal(t, 1, *p.Version)

	_, err = svc.Create(ctx, CreateRequest{Ref: SharedRef("p"), Content: "again"})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)

	p, err = svc.Update(ctx, UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)
	assert.Equal(t, 2, *p.Version)
	assert.Equal(t, "draft", p.Status)

	list, err := svc.List(ctx, ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Plans, 1)
	assert.Equal(t, "p", list.Plans[0].Name)

	require.NoError(t, svc.Delete(ctx, DeleteRequest{Ref: SharedRef("p"), ExpectedVersion: new(2)}))
	_, err = svc.Get(ctx, SharedRef("p"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestNewService_NilStoragePanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		NewService(nil)
	})
}

func TestScope_Mutable(t *testing.T) {
	t.Parallel()
	assert.True(t, ScopeShared.Mutable())
	assert.False(t, ScopeSession.Mutable())
}

// --- Test doubles --------------------------------------------------------------

// failingStorage is a plan.Storage whose every method fails, to verify the
// service classifies backend failures as *StorageError.
type failingStorage struct{ err error }

var _ plan.Storage = failingStorage{}

func (f failingStorage) Get(context.Context, string) (plan.Plan, bool, error) {
	return plan.Plan{}, false, f.err
}

func (f failingStorage) Upsert(context.Context, plan.UpsertRequest) (plan.Plan, error) {
	return plan.Plan{}, f.err
}

func (f failingStorage) List(context.Context) ([]plan.Summary, []string, error) {
	return nil, nil, f.err
}

func (f failingStorage) Delete(context.Context, string, *int) (bool, error) {
	return false, f.err
}

// memStorage is a minimal in-memory plan.Storage honouring the same contract
// as the filesystem default: Upsert owns the revision bump, the optimistic
// lock, and the must-exist guard.
type memStorage struct {
	plans map[string]plan.Plan
}

var _ plan.Storage = (*memStorage)(nil)

func newMemStorage() *memStorage {
	return &memStorage{plans: map[string]plan.Plan{}}
}

func (s *memStorage) Get(_ context.Context, name string) (plan.Plan, bool, error) {
	p, ok := s.plans[name]
	return p, ok, nil
}

func (s *memStorage) Upsert(_ context.Context, req plan.UpsertRequest) (plan.Plan, error) {
	p, exists := s.plans[req.Name]
	if req.MustExist && !exists {
		return plan.Plan{}, plan.ErrPlanNotFound
	}
	if req.ExpectedRevision != nil && p.Revision != *req.ExpectedRevision {
		return plan.Plan{}, &plan.VersionConflictError{Name: req.Name, Expected: *req.ExpectedRevision, Current: p.Revision}
	}
	p.Name = req.Name
	if req.Content != nil {
		p.Content = *req.Content
	}
	if req.Title != nil {
		p.Title = *req.Title
	}
	if req.Author != nil {
		p.Author = *req.Author
	}
	if req.Status != nil {
		p.Status = *req.Status
	}
	p.Revision++
	p.UpdatedAt = "2024-01-01T00:00:00Z"
	s.plans[req.Name] = p
	return p, nil
}

func (s *memStorage) List(context.Context) ([]plan.Summary, []string, error) {
	out := make([]plan.Summary, 0, len(s.plans))
	for name, p := range s.plans {
		out = append(out, plan.Summary{Name: name, Title: p.Title, Author: p.Author, Status: p.Status, Revision: p.Revision, UpdatedAt: p.UpdatedAt})
	}
	slices.SortFunc(out, func(a, b plan.Summary) int { return strings.Compare(a.Name, b.Name) })
	return out, nil, nil
}

func (s *memStorage) Delete(_ context.Context, name string, expectedRevision *int) (bool, error) {
	p, ok := s.plans[name]
	if !ok {
		return false, nil
	}
	if expectedRevision != nil && p.Revision != *expectedRevision {
		return false, &plan.VersionConflictError{Name: name, Expected: *expectedRevision, Current: p.Revision}
	}
	delete(s.plans, name)
	return true, nil
}
