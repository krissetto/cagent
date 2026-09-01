package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type fakeRuntime struct {
	tools.NopRuntime

	mu        sync.Mutex
	recalls   []string
	recall    bool
	recallErr error
}

func (f *fakeRuntime) EmitOutput(context.Context, string) {}

func (f *fakeRuntime) Recall(_ context.Context, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recalls = append(f.recalls, msg)
	return f.recallErr
}

func (f *fakeRuntime) Supports(c tools.Capability) bool {
	return f.recall && c == tools.CapabilityRecall
}

func (f *fakeRuntime) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.recalls...)
}

func newTestToolSet() *ToolSet {
	ts := New()
	ts.now = func() time.Time { return testNow }
	return ts
}

func TestCreateAndListSchedule(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: true}

	res, err := ts.createSchedule(t.Context(),
		CreateScheduleArgs{Prompt: "check build", When: "every:1h", Name: "ci"}, rt)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)

	list, err := ts.listSchedules(t.Context(), ListSchedulesArgs{})
	require.NoError(t, err)
	require.Contains(t, list.Output, "check build")
	require.Contains(t, list.Output, "ci")
}

func TestCreateScheduleRequiresRecall(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: false}

	res, err := ts.createSchedule(t.Context(),
		CreateScheduleArgs{Prompt: "x", When: "hourly"}, rt)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, strings.ToLower(res.Output), "recall")
	require.Empty(t, ts.store.list())
}

func TestCreateScheduleInvalidWhen(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: true}

	res, err := ts.createSchedule(t.Context(),
		CreateScheduleArgs{Prompt: "x", When: "whenever"}, rt)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Empty(t, ts.store.list())
}

func TestCreateScheduleRequiresPrompt(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: true}

	res, err := ts.createSchedule(t.Context(),
		CreateScheduleArgs{Prompt: "   ", When: "hourly"}, rt)
	require.NoError(t, err)
	require.True(t, res.IsError)
}

func TestFireDueCallsRecall(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: true}

	_, err := ts.createSchedule(t.Context(),
		CreateScheduleArgs{Prompt: "run backup", When: "every:1h", Name: "bkp"}, rt)
	require.NoError(t, err)

	ts.fireDue(t.Context(), testNow.Add(30*time.Minute))
	require.Empty(t, rt.messages())

	ts.fireDue(t.Context(), testNow.Add(time.Hour))
	msgs := rt.messages()
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0], "run backup")
	require.Contains(t, msgs[0], "bkp")
	require.Len(t, ts.store.list(), 1)
}

func TestFireDueContinuesAfterRecallError(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: true, recallErr: errors.New("host went away")}

	for _, name := range []string{"first", "second"} {
		_, err := ts.createSchedule(t.Context(),
			CreateScheduleArgs{Prompt: "do " + name, When: "every:1h", Name: name}, rt)
		require.NoError(t, err)
	}

	ts.fireDue(t.Context(), testNow.Add(time.Hour))

	require.Len(t, rt.messages(), 2)
}

func TestFireDueWithoutRuntimeKeepsSchedules(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	_, err := ts.store.add("orphan", "do it", "in:10m", testNow)
	require.NoError(t, err)

	ts.fireDue(t.Context(), testNow.Add(11*time.Minute))

	require.Len(t, ts.store.list(), 1)
}

func TestCancelScheduleTool(t *testing.T) {
	t.Parallel()

	ts := newTestToolSet()
	rt := &fakeRuntime{recall: true}

	res, _ := ts.createSchedule(t.Context(),
		CreateScheduleArgs{Prompt: "x", When: "hourly"}, rt)
	require.False(t, res.IsError)
	id := ts.store.list()[0].ID

	cres, err := ts.cancelSchedule(t.Context(), CancelScheduleArgs{ID: id})
	require.NoError(t, err)
	require.False(t, cres.IsError)
	require.Empty(t, ts.store.list())

	cres2, _ := ts.cancelSchedule(t.Context(), CancelScheduleArgs{ID: id})
	require.True(t, cres2.IsError)
}

func TestToolSetImplementsInterfaces(t *testing.T) {
	t.Parallel()

	var ts tools.ToolSet = New()

	_, ok := ts.(tools.Startable)
	require.True(t, ok, "must implement tools.Startable")
	_, ok = ts.(tools.Instructable)
	require.True(t, ok, "must implement tools.Instructable")

	toolz, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolz, 3)
}

func TestStartStopIsClean(t *testing.T) {
	t.Parallel()

	ts := New()
	require.NoError(t, ts.Start(t.Context()))
	require.NoError(t, ts.Stop(t.Context()))
}
