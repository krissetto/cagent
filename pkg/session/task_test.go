package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTask_NewTask(t *testing.T) {
	task := NewTask("Test goal", "Original user message")

	assert.NotEmpty(t, task.ID)
	assert.Equal(t, "Test goal", task.Goal)
	assert.Equal(t, "Original user message", task.OriginalMessage)
	assert.Equal(t, TaskStatusActive, task.Status)
	assert.False(t, task.CreatedAt.IsZero())
	assert.Nil(t, task.CompletedAt)
}

func TestTask_NewTask_EmptyGoal(t *testing.T) {
	// When goal is empty, it should default to original message
	task := NewTask("", "Original user message")

	assert.Equal(t, "Original user message", task.Goal)
	assert.Equal(t, "Original user message", task.OriginalMessage)
}

func TestTask_StatusTransitions(t *testing.T) {
	task := NewTask("Test goal", "Test goal")

	// Initial state
	assert.True(t, task.IsActive())
	assert.False(t, task.IsWaiting())
	assert.False(t, task.IsCompleted())

	// Transition to waiting
	task.MarkWaiting("Need user input")
	assert.False(t, task.IsActive())
	assert.True(t, task.IsWaiting())
	assert.False(t, task.IsCompleted())
	assert.Equal(t, "Need user input", task.WaitingQuestion)

	// Resume from waiting
	task.Resume()
	assert.True(t, task.IsActive())
	assert.False(t, task.IsWaiting())
	assert.Empty(t, task.WaitingQuestion)

	// Complete the task
	task.MarkCompleted("Final response", "Task summary")
	assert.False(t, task.IsActive())
	assert.False(t, task.IsWaiting())
	assert.True(t, task.IsCompleted())
	assert.Equal(t, "Final response", task.FinalResponse)
	assert.Equal(t, "Task summary", task.Summary)
	assert.NotNil(t, task.CompletedAt)
}

func TestTask_UpdateState(t *testing.T) {
	task := NewTask("Test goal", "Test goal")

	task.UpdateState("Step 1 complete")
	assert.Equal(t, "Step 1 complete", task.State)

	task.UpdateState("Step 2 in progress")
	assert.Equal(t, "Step 2 in progress", task.State)
}

func TestTask_AddUsage(t *testing.T) {
	task := NewTask("Test goal", "Test goal")

	// Initially all counters should be zero
	assert.Zero(t, task.InputTokens)
	assert.Zero(t, task.OutputTokens)
	assert.Zero(t, task.CachedInputTokens)
	assert.Zero(t, task.CacheWriteTokens)
	assert.Zero(t, task.Cost)

	// Add usage
	task.AddUsage(100, 50, 20, 10, 0.005)

	assert.Equal(t, int64(100), task.InputTokens)
	assert.Equal(t, int64(50), task.OutputTokens)
	assert.Equal(t, int64(20), task.CachedInputTokens)
	assert.Equal(t, int64(10), task.CacheWriteTokens)
	assert.InDelta(t, 0.005, task.Cost, 0.0001)

	// Add more usage (should accumulate)
	task.AddUsage(200, 100, 30, 15, 0.01)

	assert.Equal(t, int64(300), task.InputTokens)
	assert.Equal(t, int64(150), task.OutputTokens)
	assert.Equal(t, int64(50), task.CachedInputTokens)
	assert.Equal(t, int64(25), task.CacheWriteTokens)
	assert.InDelta(t, 0.015, task.Cost, 0.0001)
}

func TestTask_TotalTokens(t *testing.T) {
	task := NewTask("Test goal", "Test goal")

	task.AddUsage(100, 50, 20, 10, 0.005)

	// TotalInputTokens: InputTokens + CachedInputTokens + CacheWriteTokens = 100 + 20 + 10 = 130
	assert.Equal(t, int64(130), task.TotalInputTokens())

	// TotalTokens: TotalInputTokens + OutputTokens = 130 + 50 = 180
	assert.Equal(t, int64(180), task.TotalTokens())
}

func TestTaskContext(t *testing.T) {
	t.Parallel()

	task := NewTask("Test goal", "Test goal")

	// Initially context should have no task
	ctx := t.Context()
	assert.Nil(t, TaskFromContext(ctx))

	// Add task to context
	ctxWithTask := WithActiveTask(ctx, task)
	retrieved := TaskFromContext(ctxWithTask)

	assert.NotNil(t, retrieved)
	assert.Equal(t, task.ID, retrieved.ID)
	assert.Equal(t, task.Goal, retrieved.Goal)

	// Original context should still have no task
	assert.Nil(t, TaskFromContext(ctx))
}

func TestSession_TaskPersistence(t *testing.T) {
	// Create a temporary database
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	store, err := NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	sqliteStore := store.(*SQLiteSessionStore)
	defer func() {
		_ = sqliteStore.Close()
		os.Remove(dbPath)
	}()

	ctx := t.Context()

	// Create session with tasks
	sess := New()
	sess.Title = "Test Session"

	// Add a completed task
	task1 := sess.StartTask("First task goal", "First task goal")
	task1.UpdateState("Working on it")
	task1.MarkCompleted("First response", "First summary")
	sess.ClearActiveTask()

	// Add an active task
	task2 := sess.StartTask("Second task goal", "Second task goal")
	task2.UpdateState("In progress")

	// Persist session
	err = store.AddSession(ctx, sess)
	require.NoError(t, err)

	// Load session
	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)

	// Verify tasks were persisted
	require.Len(t, loaded.Tasks, 2)

	// Verify first task
	loadedTask1 := loaded.Tasks[0]
	assert.Equal(t, task1.ID, loadedTask1.ID)
	assert.Equal(t, task1.Goal, loadedTask1.Goal)
	assert.Equal(t, TaskStatusCompleted, loadedTask1.Status)
	assert.Equal(t, "First response", loadedTask1.FinalResponse)
	assert.Equal(t, "First summary", loadedTask1.Summary)

	// Verify second task
	loadedTask2 := loaded.Tasks[1]
	assert.Equal(t, task2.ID, loadedTask2.ID)
	assert.Equal(t, task2.Goal, loadedTask2.Goal)
	assert.Equal(t, TaskStatusActive, loadedTask2.Status)
	assert.Equal(t, "In progress", loadedTask2.State)

	// Verify active task ID
	assert.Equal(t, task2.ID, loaded.ActiveTaskID)
	assert.Equal(t, loadedTask2, loaded.ActiveTask())
}

func TestSession_TaskUsagePersistence(t *testing.T) {
	// Create a temporary database
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	store, err := NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	sqliteStore := store.(*SQLiteSessionStore)
	defer func() {
		_ = sqliteStore.Close()
	}()

	ctx := t.Context()

	// Create session with a task that has usage data
	sess := New()
	sess.Title = "Test Session"

	task := sess.StartTask("Task with usage", "Task with usage")
	task.AddUsage(1000, 500, 200, 100, 0.05)
	task.MarkCompleted("Response", "Summary")
	sess.ClearActiveTask()

	// Persist session
	err = store.UpdateSession(ctx, sess)
	require.NoError(t, err)

	// Load session
	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)

	// Verify task usage was persisted
	require.Len(t, loaded.Tasks, 1)
	loadedTask := loaded.Tasks[0]

	assert.Equal(t, int64(1000), loadedTask.InputTokens)
	assert.Equal(t, int64(500), loadedTask.OutputTokens)
	assert.Equal(t, int64(200), loadedTask.CachedInputTokens)
	assert.Equal(t, int64(100), loadedTask.CacheWriteTokens)
	assert.InDelta(t, 0.05, loadedTask.Cost, 0.0001)
}

func TestSession_TaskSummaries(t *testing.T) {
	sess := New()

	// Add 5 completed tasks
	for i := 1; i <= 5; i++ {
		goalStr := "Goal " + string(rune('A'+i-1))
		task := sess.StartTask(goalStr, goalStr)
		task.MarkCompleted("Response", "Summary "+string(rune('A'+i-1)))
		sess.ClearActiveTask()
	}

	// Default should return 3 summaries
	summaries := sess.RecentTaskSummaries()
	assert.Len(t, summaries, 3)
	assert.Equal(t, "Summary C", summaries[0])
	assert.Equal(t, "Summary D", summaries[1])
	assert.Equal(t, "Summary E", summaries[2])

	// Change count
	sess.TaskSummaryCount = 5
	summaries = sess.RecentTaskSummaries()
	assert.Len(t, summaries, 5)
}

func TestSession_UpdateSession_WithTasks(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	store, err := NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	sqliteStore := store.(*SQLiteSessionStore)
	defer func() {
		_ = sqliteStore.Close()
		os.Remove(dbPath)
	}()

	ctx := t.Context()

	// Create and save session
	sess := New()
	sess.Title = "Test Session"
	task := sess.StartTask("Test goal", "Test goal")

	err = store.UpdateSession(ctx, sess)
	require.NoError(t, err)

	// Update the task
	task.UpdateState("Updated state")
	task.MarkCompleted("Response", "Summary")
	sess.ClearActiveTask()

	err = store.UpdateSession(ctx, sess)
	require.NoError(t, err)

	// Reload and verify
	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)

	require.Len(t, loaded.Tasks, 1)
	loadedTask := loaded.Tasks[0]
	assert.Equal(t, TaskStatusCompleted, loadedTask.Status)
	assert.Equal(t, "Updated state", loadedTask.State)
	assert.Equal(t, "Response", loadedTask.FinalResponse)
	assert.Empty(t, loaded.ActiveTaskID)
}
