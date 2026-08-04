package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

func TestSubscribeChanges_WriteEmitsOnSuccess(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	rec := &changeRecorder{}
	tool.SubscribeChanges(rec.callback())

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

func TestSubscribeChanges_UpdateFromFileEmitsWrite(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	rec := &changeRecorder{}
	tool.SubscribeChanges(rec.callback())

	path := writeTempFile(t, "content from file")
	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: path})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionWrite, Revision: 1}, rec.changes[0])
}

func TestSubscribeChanges_SetStatusEmitsStatus(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SubscribeChanges(rec.callback())

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: "done"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionStatus, Revision: 2}, rec.changes[0])
}

func TestSubscribeChanges_DeleteEmitsDelete(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SubscribeChanges(rec.callback())

	rev := 1
	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p", LastKnownRevision: &rev})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionDelete, Revision: 1}, rec.changes[0])
}

func TestSubscribeChanges_UnguardedDeleteEmitsZeroRevision(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SubscribeChanges(rec.callback())

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, rec.changes, 1)
	assert.Equal(t, Change{Name: "p", Action: ChangeActionDelete, Revision: 0}, rec.changes[0])
}

func TestSubscribeChanges_NoEmissionOnFailure(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	rec := &changeRecorder{}
	tool.SubscribeChanges(rec.callback())

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

func TestSubscribeChanges_MultipleSubscribersEachReceiveOnce(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	recA := &changeRecorder{}
	recB := &changeRecorder{}
	recC := &changeRecorder{}
	tool.SubscribeChanges(recA.callback())
	tool.SubscribeChanges(recB.callback())
	tool.SubscribeChanges(recC.callback())

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	want := Change{Name: "p", Action: ChangeActionWrite, Revision: 1}
	for name, rec := range map[string]*changeRecorder{"A": recA, "B": recB, "C": recC} {
		require.Len(t, rec.changes, 1, "subscriber %s must receive the change exactly once", name)
		assert.Equal(t, want, rec.changes[0], "subscriber %s", name)
	}
}

func TestSubscribeChanges_UnsubscribeIsIsolatedAndIdempotent(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	recA := &changeRecorder{}
	recB := &changeRecorder{}
	unsubA := tool.SubscribeChanges(recA.callback())
	unsubB := tool.SubscribeChanges(recB.callback())

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.Len(t, recA.changes, 1)
	require.Len(t, recB.changes, 1)

	unsubA()
	unsubA() // idempotent: a second call must not disturb other subscriptions

	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)
	assert.Len(t, recA.changes, 1, "unsubscribed A must not be called again")
	require.Len(t, recB.changes, 2, "B must keep receiving after A unsubscribed")

	unsubB()
	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v3"})
	require.NoError(t, err)
	assert.Len(t, recA.changes, 1)
	assert.Len(t, recB.changes, 2)
}

func TestSubscribeChanges_NilCallbackIsSafe(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	// Never subscribed: writes succeed without any subscriber.
	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	// A nil callback registers nothing and yields a callable unsubscribe.
	unsub := tool.SubscribeChanges(nil)
	require.NotNil(t, unsub)
	unsub()
	unsub()

	result, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// TestSubscribeChanges_ConcurrentSubscribeUnsubscribeNotify hammers the
// registry from three directions at once; run with -race to prove the
// subscription lifecycle is race-clean. Note: an in-flight notification that
// snapshotted the subscribers before an unsubscribe may still deliver once
// (copy-then-invoke keeps callbacks out of the registry lock), so this test
// asserts race-cleanliness and exact delivery to a steady subscriber, not
// cut-off timing for churning ones.
func TestSubscribeChanges_ConcurrentSubscribeUnsubscribeNotify(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	const (
		writers     = 4
		writesPerGo = 10
		churners    = 4
		churnsPerGo = 25
	)

	var steady atomic.Int64
	unsubSteady := tool.SubscribeChanges(func(Change) { steady.Add(1) })
	defer unsubSteady()

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range writesPerGo {
				name := fmt.Sprintf("plan-%d", w)
				_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: name, Content: fmt.Sprintf("v%d", i)})
				assert.NoError(t, err)
			}
		})
	}
	for range churners {
		wg.Go(func() {
			for range churnsPerGo {
				var count atomic.Int64
				unsub := tool.SubscribeChanges(func(Change) { count.Add(1) })
				unsub()
				unsub() // idempotent under concurrency too
			}
		})
	}
	wg.Wait()

	assert.Equal(t, int64(writers*writesPerGo), steady.Load(),
		"the steady subscriber must see every successful write exactly once")
}

func TestChangeNotifier_DiscoverableThroughWrapper(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	wrapped := tools.NewStartable(tool)

	notifier, ok := tools.As[ChangeNotifier](wrapped)
	require.True(t, ok, "ChangeNotifier must be reachable through toolset wrappers")

	rec := &changeRecorder{}
	unsub := notifier.SubscribeChanges(rec.callback())
	defer unsub()

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	require.Len(t, rec.changes, 1)
}
