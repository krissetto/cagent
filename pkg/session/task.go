package session

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// taskContextKey is the context key for propagating the active task
type taskContextKey struct{}

// WithActiveTask returns a new context with the given task attached.
// This allows model usage to be attributed to the task across runtime calls.
func WithActiveTask(ctx context.Context, task *Task) context.Context {
	return context.WithValue(ctx, taskContextKey{}, task)
}

// TaskFromContext retrieves the active task from the context, or nil if none.
func TaskFromContext(ctx context.Context) *Task {
	if task, ok := ctx.Value(taskContextKey{}).(*Task); ok {
		return task
	}
	return nil
}

// TaskStatus represents the current state of a task
type TaskStatus string

const (
	TaskStatusActive    TaskStatus = "active"
	TaskStatusWaiting   TaskStatus = "waiting" // Waiting for user input
	TaskStatusCompleted TaskStatus = "completed"
)

// Task represents a unit of work in the task-based context runtime.
// Tasks have their own lifecycle and state, separate from the full chat history.
type Task struct {
	// ID is the unique identifier for this task
	ID string `json:"id"`

	// Goal is an LLM-generated concise description of the task objective
	Goal string `json:"goal"`

	// OriginalMessage is the raw user message that initiated this task
	OriginalMessage string `json:"original_message,omitempty"`

	// Status indicates the current state of the task
	Status TaskStatus `json:"status"`

	// State is a compact representation of the current task progress,
	// updated by the agent via task_update_state tool
	State string `json:"state,omitempty"`

	// WaitingQuestion is the question posed to the user when status is "waiting"
	WaitingQuestion string `json:"waiting_question,omitempty"`

	// FinalResponse is the agent's final response when the task is completed
	FinalResponse string `json:"final_response,omitempty"`

	// Summary is a brief summary of the completed task for context in future tasks
	Summary string `json:"summary,omitempty"`

	// CreatedAt is when the task was created
	CreatedAt time.Time `json:"created_at"`

	// CompletedAt is when the task was completed (if completed)
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// Usage tracking fields for per-task cost/token accounting
	InputTokens       int64   `json:"input_tokens,omitempty"`
	OutputTokens      int64   `json:"output_tokens,omitempty"`
	CachedInputTokens int64   `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int64   `json:"cache_write_tokens,omitempty"`
	Cost              float64 `json:"cost,omitempty"`
}

// NewTask creates a new task with the given goal and original message.
// If goal is empty, it defaults to the originalMessage.
func NewTask(goal, originalMessage string) *Task {
	if goal == "" {
		goal = originalMessage
	}
	return &Task{
		ID:              uuid.New().String(),
		Goal:            goal,
		OriginalMessage: originalMessage,
		Status:          TaskStatusActive,
		CreatedAt:       time.Now(),
	}
}

// IsActive returns true if the task is still active (not completed or waiting)
func (t *Task) IsActive() bool {
	return t.Status == TaskStatusActive
}

// IsWaiting returns true if the task is waiting for user input
func (t *Task) IsWaiting() bool {
	return t.Status == TaskStatusWaiting
}

// IsCompleted returns true if the task has been completed
func (t *Task) IsCompleted() bool {
	return t.Status == TaskStatusCompleted
}

// MarkWaiting marks the task as waiting for user input with the given question
func (t *Task) MarkWaiting(question string) {
	t.Status = TaskStatusWaiting
	t.WaitingQuestion = question
}

// MarkCompleted marks the task as completed with the given response and summary
func (t *Task) MarkCompleted(response, summary string) {
	t.Status = TaskStatusCompleted
	t.FinalResponse = response
	t.Summary = summary
	now := time.Now()
	t.CompletedAt = &now
}

// Resume resumes a waiting task back to active status
func (t *Task) Resume() {
	if t.Status == TaskStatusWaiting {
		t.Status = TaskStatusActive
		t.WaitingQuestion = ""
	}
}

// UpdateState updates the compact state representation
func (t *Task) UpdateState(state string) {
	t.State = state
}

// AddUsage adds token usage and cost to the task's running totals.
// This is called after each model stream completes to attribute usage to this task.
func (t *Task) AddUsage(inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int64, cost float64) {
	t.InputTokens += inputTokens
	t.OutputTokens += outputTokens
	t.CachedInputTokens += cachedInputTokens
	t.CacheWriteTokens += cacheWriteTokens
	t.Cost += cost
}

// TotalInputTokens returns the total input tokens including cached and cache write tokens
func (t *Task) TotalInputTokens() int64 {
	return t.InputTokens + t.CachedInputTokens + t.CacheWriteTokens
}

// TotalTokens returns the sum of all input (including cached) and output tokens
func (t *Task) TotalTokens() int64 {
	return t.TotalInputTokens() + t.OutputTokens
}
