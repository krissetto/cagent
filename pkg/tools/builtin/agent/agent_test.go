package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockRunner implements Runner for testing.
type mockRunner struct {
	subAgentNames []string
	runResult     *RunResult
	runDelay      time.Duration // optional delay to simulate work
	runFunc       func(ctx context.Context, params RunParams) *RunResult
}

func (m *mockRunner) CurrentAgentSubAgentNames() []string { return m.subAgentNames }
func (m *mockRunner) RunAgent(ctx context.Context, params RunParams) *RunResult {
	if m.runFunc != nil {
		return m.runFunc(ctx, params)
	}
	if m.runDelay > 0 {
		select {
		case <-time.After(m.runDelay):
		case <-ctx.Done():
			return &RunResult{Stopped: true}
		}
	}
	// Call OnContent if result has content, to simulate streaming.
	if m.runResult != nil && m.runResult.Result != "" && params.OnContent != nil {
		params.OnContent(m.runResult.Result)
	}
	if m.runResult != nil {
		return m.runResult
	}
	return &RunResult{}
}

func newTestHandler() *Handler {
	return &Handler{tasks: concurrent.NewMap[string, *task]()}
}

func newTestHandlerWithRunner(r Runner) *Handler {
	return NewHandler(r)
}

func insertTask(h *Handler, id, agentName string, status taskStatus) *task {
	t := &task{
		id:        id,
		agentName: agentName,
		taskDesc:  "test task",
		cancel:    func() {},
		startTime: time.Now(),
	}
	t.status.Store(int32(status))
	h.tasks.Store(id, t)
	return t
}

func makeToolCall(t *testing.T, args any) tools.ToolCall {
	t.Helper()
	b, err := json.Marshal(args)
	require.NoError(t, err)
	return tools.ToolCall{Function: tools.FunctionCall{Arguments: string(b)}}
}

// --- newTaskID ---

func TestNewTaskID_IsUnique(t *testing.T) {
	ids := make(map[string]struct{})
	for range 100 {
		id := newTaskID()
		assert.NotEmpty(t, id)
		_, dup := ids[id]
		assert.False(t, dup, "duplicate task ID: %s", id)
		ids[id] = struct{}{}
	}
}

func TestNewTaskID_HasPrefix(t *testing.T) {
	id := newTaskID()
	assert.True(t, strings.HasPrefix(id, "agent_task_"), "ID should start with agent_task_ prefix, got: %s", id)
}

// --- statusToString ---

func TestStatusToString(t *testing.T) {
	cases := []struct {
		status   taskStatus
		expected string
	}{
		{taskRunning, "running"},
		{taskCompleted, "completed"},
		{taskStopped, "stopped"},
		{taskFailed, "failed"},
		{99, "unknown"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.expected, statusToString(tc.status))
	}
}

// --- runningTaskCount / totalTaskCount ---

func TestTaskCounts(t *testing.T) {
	h := newTestHandler()
	assert.Equal(t, 0, h.runningTaskCount())
	assert.Equal(t, 0, h.totalTaskCount())

	insertTask(h, "t1", "a", taskRunning)
	insertTask(h, "t2", "b", taskRunning)
	insertTask(h, "t3", "c", taskCompleted)
	insertTask(h, "t4", "d", taskFailed)

	assert.Equal(t, 2, h.runningTaskCount())
	assert.Equal(t, 4, h.totalTaskCount())
}

// --- pruneCompleted ---

func TestPruneCompleted(t *testing.T) {
	h := newTestHandler()
	insertTask(h, "run1", "a", taskRunning)
	insertTask(h, "done1", "b", taskCompleted)
	insertTask(h, "done2", "c", taskStopped)
	insertTask(h, "fail1", "d", taskFailed)

	h.pruneCompleted()

	assert.Equal(t, 1, h.totalTaskCount())
	_, exists := h.tasks.Load("run1")
	assert.True(t, exists, "running task should be kept")
	_, exists = h.tasks.Load("done1")
	assert.False(t, exists, "completed task should be pruned")
}

// --- HandleList ---

func TestHandleList_Empty(t *testing.T) {
	h := newTestHandler()
	result, err := h.HandleList(t.Context(), nil, tools.ToolCall{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "No background agent tasks found")
}

func TestHandleList_ShowsTasks(t *testing.T) {
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskRunning)
	insertTask(h, "t2", "writer", taskCompleted)

	result, err := h.HandleList(t.Context(), nil, tools.ToolCall{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "researcher")
	assert.Contains(t, result.Output, "writer")
	assert.Contains(t, result.Output, "running")
	assert.Contains(t, result.Output, "completed")
}

// --- HandleView ---

func TestHandleView_NotFound(t *testing.T) {
	h := newTestHandler()
	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "nonexistent"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestHandleView_Completed(t *testing.T) {
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskCompleted)
	tk.result = "Here is my research."

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "Here is my research.")
	assert.Contains(t, result.Output, "completed")
}

func TestHandleView_Failed(t *testing.T) {
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskFailed)
	tk.errMsg = "model unavailable"

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "task failed")
	assert.Contains(t, result.Output, "model unavailable")
}

func TestHandleView_Running_NoOutputYet(t *testing.T) {
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskRunning)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "no output yet")
}

func TestHandleView_Running_WithProgress(t *testing.T) {
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskRunning)
	tk.output.WriteString("Partial research so far...")

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Partial research so far...")
	assert.Contains(t, result.Output, "still running")
}

func TestHandleView_Stopped(t *testing.T) {
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskStopped)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "stopped")
	assert.Contains(t, result.Output, "task was stopped")
}

func TestHandleView_Completed_EmptyResult(t *testing.T) {
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskCompleted)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "no output")
}

func TestHandleView_CompletedResultTruncatedToMaxOutputBytes(t *testing.T) {
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskCompleted)
	tk.result = capCompletedResult(strings.Repeat("x", maxOutputBytes+2048))

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Len(t, tk.result, maxOutputBytes)
	assert.Contains(t, result.Output, "Task ID: t1")
	assert.Contains(t, result.Output, "Agent:   researcher")
	assert.Contains(t, result.Output, "Status:  completed")
	assert.Contains(t, result.Output, "--- Output ---")
	assert.Contains(t, result.Output, "[output truncated at 10MB limit]")
	assert.Contains(t, result.Output, strings.Repeat("x", 1024))
}

func TestHandleView_InvalidJSON(t *testing.T) {
	h := newTestHandler()
	bad := tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}}
	_, err := h.HandleView(t.Context(), nil, bad)
	require.Error(t, err, "invalid JSON should return an error")
}

// --- HandleStop ---

func TestHandleStop_NotFound(t *testing.T) {
	h := newTestHandler()
	tc := makeToolCall(t, StopBackgroundAgentArgs{TaskID: "ghost"})
	result, err := h.HandleStop(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestHandleStop_AlreadyCompleted(t *testing.T) {
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskCompleted)

	tc := makeToolCall(t, StopBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleStop(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not running")
}

func TestHandleStop_Running(t *testing.T) {
	h := newTestHandler()
	cancelled := false
	tk := insertTask(h, "t1", "researcher", taskRunning)
	tk.cancel = func() { cancelled = true }

	tc := makeToolCall(t, StopBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleStop(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.True(t, cancelled)
	assert.Equal(t, taskStopped, tk.loadStatus())
}

func TestHandleStop_ExplicitStopWinsOverLateSuccessfulReturn(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runFunc: func(ctx context.Context, params RunParams) *RunResult {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			return &RunResult{Result: "late success"}
		},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "long task"})
	_, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)

	var taskID string
	h.tasks.Range(func(id string, _ *task) bool {
		taskID = id
		return false
	})
	require.NotEmpty(t, taskID)

	stopTC := makeToolCall(t, StopBackgroundAgentArgs{TaskID: taskID})
	result, err := h.HandleStop(t.Context(), nil, stopTC)
	require.NoError(t, err)
	assert.False(t, result.IsError)

	h.wg.Wait()

	tk, ok := h.tasks.Load(taskID)
	require.True(t, ok)
	assert.Equal(t, taskStopped, tk.loadStatus())

	viewTC := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: taskID})
	viewResult, err := h.HandleView(t.Context(), nil, viewTC)
	require.NoError(t, err)
	assert.Contains(t, viewResult.Output, "Status:  stopped")
	assert.Contains(t, viewResult.Output, "<task was stopped>")
	assert.NotContains(t, viewResult.Output, "late success")
}

func TestHandleStop_InvalidJSON(t *testing.T) {
	h := newTestHandler()
	bad := tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}}
	_, err := h.HandleStop(t.Context(), nil, bad)
	require.Error(t, err, "invalid JSON should return an error")
}

// --- StopAll waits for goroutines ---

func TestStopAll_WaitsForGoroutines(t *testing.T) {
	h := newTestHandler()

	var goroutineExited atomic.Bool
	tk := insertTask(h, "t1", "researcher", taskRunning)
	ctx, cancel := context.WithCancel(t.Context())
	tk.cancel = cancel

	h.wg.Go(func() {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // simulate teardown work
		goroutineExited.Store(true)
	})

	h.StopAll()
	assert.True(t, goroutineExited.Load(), "StopAll should wait for goroutine to exit")
}

// --- HandleRun: input validation ---

func TestHandleRun_AfterStopAllRejectsNewTasks(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})

	h.StopAll()

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "handler is stopped")
	assert.Equal(t, 0, h.totalTaskCount())
}

func TestHandleRun_EmptyAgent(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "agent name must not be empty")
}

func TestHandleRun_EmptyTask(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: ""})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task must not be empty")
}

func TestHandleRun_InvalidSubAgent(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "nonexistent", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not in the sub-agents list")
}

func TestHandleRun_NoSubAgents(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: nil})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "some-agent", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "no sub-agents configured")
}

func TestHandleRun_ConcurrencyCapEnforced(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})

	for i := range maxConcurrentTasks {
		insertTask(h, "fake"+string(rune('a'+i)), "sub", taskRunning)
	}

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "maximum concurrent")
}

func TestHandleRun_InvalidJSON(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	bad := tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}}
	_, err := h.HandleRun(t.Context(), session.New(), bad)
	require.Error(t, err, "invalid JSON should return an error")
}

func TestHandleRun_ContextCancellationDoesNotStopBackgroundTask(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runFunc: func(ctx context.Context, params RunParams) *RunResult {
			<-time.After(20 * time.Millisecond)
			return &RunResult{Result: "done"}
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "write a poem"})
	result, err := h.HandleRun(ctx, session.New(), tc)
	require.NoError(t, err)
	cancel()
	assert.False(t, result.IsError)

	h.wg.Wait()
	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskCompleted, tk.loadStatus())
		assert.Equal(t, "done", tk.result)
		return true
	})
}

func TestHandleRun_OversizedContentChunkIsTruncatedAtWriteTime(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runFunc: func(ctx context.Context, params RunParams) *RunResult {
			params.OnContent(strings.Repeat("x", maxOutputBytes+1024))
			return &RunResult{Result: "done"}
		},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "stream output"})
	_, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)

	h.wg.Wait()
	h.tasks.Range(func(_ string, tk *task) bool {
		tk.outputMu.RLock()
		defer tk.outputMu.RUnlock()
		assert.Equal(t, maxOutputBytes, tk.outputBytes)
		assert.Len(t, tk.output.String(), maxOutputBytes)
		return true
	})
}

func TestHandleRun_CompletedResultLargerThanMaxOutputBytesIsTruncated(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: strings.Repeat("y", maxOutputBytes+4096)},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "produce final output"})
	_, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)

	h.wg.Wait()
	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskCompleted, tk.loadStatus())
		assert.Len(t, tk.result, maxOutputBytes)
		assert.Contains(t, tk.result, "[output truncated at 10MB limit]")
		return true
	})
}

func TestHandleRun_StopAllMarksCanceledTaskStopped(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runFunc: func(ctx context.Context, params RunParams) *RunResult {
			<-ctx.Done()
			return &RunResult{Stopped: true}
		},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "long task"})
	_, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)

	h.StopAll()
	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskStopped, tk.loadStatus())
		return true
	})
}

func TestHandleRun_AtomicConcurrentAdmissionRespectsCap(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runFunc: func(ctx context.Context, params RunParams) *RunResult {
			<-ctx.Done()
			return &RunResult{Stopped: true}
		},
	})

	callTC := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "work"})

	var wg sync.WaitGroup
	results := make(chan bool, maxConcurrentTasks+10)
	for range maxConcurrentTasks + 10 {
		wg.Go(func() {
			res, err := h.HandleRun(t.Context(), session.New(), callTC)
			if err == nil {
				results <- !res.IsError
				return
			}
			results <- false
		})
	}
	wg.Wait()
	close(results)

	var started int
	for ok := range results {
		if ok {
			started++
		}
	}
	assert.Equal(t, maxConcurrentTasks, started)
	assert.Equal(t, maxConcurrentTasks, h.runningTaskCount())
	assert.Equal(t, maxConcurrentTasks, h.totalTaskCount())

	h.StopAll()
}

func TestHandleRun_ProviderError_TaskFails(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{ErrMsg: "model unavailable"},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError, "HandleRun should start successfully before provider error")

	h.wg.Wait()

	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskFailed, tk.loadStatus(), "task should be marked failed on provider error")
		assert.NotEmpty(t, tk.errMsg)
		return true
	})
}

func TestHandleRun_WithExpectedOutput(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: "result"},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{
		Agent:          "sub",
		Task:           "summarize the document",
		ExpectedOutput: "A one-paragraph summary",
	})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)

	h.wg.Wait()

	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskCompleted, tk.loadStatus())
		return true
	})
}

func TestHandleRun_ForwardsParentSessionAndExpectedOutput(t *testing.T) {
	parent := session.New(session.WithUserMessage("start"))

	var gotParams RunParams
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runFunc: func(ctx context.Context, params RunParams) *RunResult {
			gotParams = params
			return &RunResult{Result: "done"}
		},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{
		Agent:          "sub",
		Task:           "summarize the document",
		ExpectedOutput: "A one-paragraph summary",
	})
	result, err := h.HandleRun(t.Context(), parent, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)

	h.wg.Wait()

	assert.Equal(t, "sub", gotParams.AgentName)
	assert.Equal(t, "summarize the document", gotParams.Task)
	assert.Equal(t, "A one-paragraph summary", gotParams.ExpectedOutput)
	assert.Same(t, parent, gotParams.ParentSession)
	assert.NotNil(t, gotParams.OnContent)
}

func TestHandleRun_TotalCapAutoPruneAdmits(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: "done"},
	})

	for i := range maxTotalTasks {
		insertTask(h, fmt.Sprintf("done%d", i), "sub", taskCompleted)
	}
	assert.Equal(t, maxTotalTasks, h.totalTaskCount())

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError, "task should be admitted after auto-prune of completed tasks")

	h.wg.Wait()
}

func TestHandleRun_TotalCapExhaustion_ConcurrencyCapFiresFirst(t *testing.T) {
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})

	for i := range maxConcurrentTasks {
		insertTask(h, fmt.Sprintf("run%d", i), "sub", taskRunning)
	}

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "maximum concurrent",
		"concurrency cap should fire before total cap can be exhausted non-prunably")
}

// --- Concurrent handler access (run with -race) ---

func TestHandler_ConcurrentAccess(t *testing.T) {
	h := newTestHandler()

	for i := range 10 {
		tk := insertTask(h, fmt.Sprintf("task%d", i), "researcher", taskRunning)
		tk.output.WriteString("some progress output")
		tk.outputBytes = len("some progress output")
	}

	viewTCs := make([]tools.ToolCall, 5)
	for i := range 5 {
		viewTCs[i] = makeToolCall(t, ViewBackgroundAgentArgs{TaskID: fmt.Sprintf("task%d", i%10)})
	}
	stopTCs := make([]tools.ToolCall, 3)
	for i := range 3 {
		stopTCs[i] = makeToolCall(t, StopBackgroundAgentArgs{TaskID: fmt.Sprintf("task%d", i)})
	}

	var wg sync.WaitGroup

	for range 5 {
		wg.Go(func() {
			_, _ = h.HandleList(t.Context(), nil, tools.ToolCall{})
		})
	}

	for i := range 5 {
		tc := viewTCs[i]
		wg.Go(func() {
			_, _ = h.HandleView(t.Context(), nil, tc)
		})
	}

	for i := range 3 {
		tc := stopTCs[i]
		wg.Go(func() {
			_, _ = h.HandleStop(t.Context(), nil, tc)
		})
	}

	wg.Wait()
	assert.LessOrEqual(t, h.runningTaskCount(), 10)
}

// --- Tools ---

func TestNewToolSet_ReturnsFourTools(t *testing.T) {
	ts := NewToolSet()
	toolsList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, toolsList, 4)

	names := make([]string, len(toolsList))
	for i, tl := range toolsList {
		names[i] = tl.Name
	}
	assert.Contains(t, names, ToolNameRunBackgroundAgent)
	assert.Contains(t, names, ToolNameListBackgroundAgents)
	assert.Contains(t, names, ToolNameViewBackgroundAgent)
	assert.Contains(t, names, ToolNameStopBackgroundAgent)
}

func TestNewToolSet_Instructions(t *testing.T) {
	ts := NewToolSet()
	instructable, ok := ts.(tools.Instructable)
	require.True(t, ok, "NewToolSet should implement Instructable")

	instructions := instructable.Instructions()
	assert.NotEmpty(t, instructions)
	assert.Contains(t, instructions, "run_background_agent")
	assert.Contains(t, instructions, "list_background_agents")
	assert.Contains(t, instructions, "view_background_agent")
	assert.Contains(t, instructions, "stop_background_agent")
}
