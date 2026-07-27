package plan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// changeRecorder collects Change notifications for assertions.
type changeRecorder struct {
	changes []Change
}

func (r *changeRecorder) callback() func(Change) {
	return func(c Change) { r.changes = append(r.changes, c) }
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "content.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestChangeCallback_WriteEmitsOnSuccess(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionWrite, Revision: 1}, rec.changes[0])

	result, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, rec.changes, 2)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionWrite, Revision: 2}, rec.changes[1])
}

func TestChangeCallback_UpdateFromFileEmitsWrite(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())

	path := writeTempFile(t, "content from file")
	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: path})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionWrite, Revision: 1}, rec.changes[0])
}

func TestChangeCallback_SetStatusEmitsStatus(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: "done"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionStatus, Revision: 2}, rec.changes[0])
}

func TestChangeCallback_DeleteEmitsDelete(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())

	rev := 1
	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p", LastKnownRevision: &rev})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionDelete, Revision: 1}, rec.changes[0])
}

func TestChangeCallback_UnguardedDeleteEmitsZeroRevision(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionDelete, Revision: 0}, rec.changes[0])
}

func TestChangeCallback_NoEmissionOnFailure(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())

	// Empty content: validation failure.
	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: ""})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	// Version conflict.
	stale := 42
	result, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2", LastKnownRevision: &stale})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	result, err = tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: "done", LastKnownRevision: &stale})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	// Deleting a missing plan.
	result, err = tool.deletePlan(t.Context(), DeletePlanArgs{Name: "missing"})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	assert.Empty(t, rec.changes, "failed writes must not notify")
}

func TestChangeCallback_NilCallbackIsSafe(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	// Never set: writes succeed without a callback.
	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	// Set then unregister.
	rec := &changeRecorder{}
	tool.SetChangeCallback(rec.callback())
	tool.SetChangeCallback(nil)

	result, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Empty(t, rec.changes)
}

func TestChangeNotifier_DiscoverableThroughWrapper(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	wrapped := tools.NewStartable(tool)

	notifier, ok := tools.As[ChangeNotifier](wrapped)
	require.True(t, ok, "ChangeNotifier must be reachable through toolset wrappers")

	rec := &changeRecorder{}
	notifier.SetChangeCallback(rec.callback())

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.Len(t, rec.changes, 1)
}
