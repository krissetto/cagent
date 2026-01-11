package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/tools/builtin"
)

func TestTaskPromptBuilder_BuildMessages(t *testing.T) {
	// Create a session with some task history
	sess := session.New()
	sess.WorkingDir = "/test/dir"

	// Add some completed tasks with summaries
	task1 := sess.StartTask("First task goal", "First task goal")
	task1.MarkCompleted("First response", "Summary of first task")
	sess.ClearActiveTask()

	task2 := sess.StartTask("Second task goal", "Second task goal")
	task2.MarkCompleted("Second response", "Summary of second task")
	sess.ClearActiveTask()

	// Start a new active task
	activeTask := sess.StartTask("Current task goal", "Current task goal")

	// Create a simple agent for testing
	prov := &mockProvider{id: "test/mock-model"}
	testAgent := agent.New("test", "You are a test agent", agent.WithModel(prov))

	builder := NewTaskPromptBuilder(sess, testAgent)
	messages := builder.BuildMessages(nil)

	// Verify message structure
	require.GreaterOrEqual(t, len(messages), 3, "Should have at least system, task summary, and user messages")

	// Check that we have a system message with agent instructions
	hasSystemInstruction := false
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleSystem && msg.Content != "" {
			hasSystemInstruction = true
			break
		}
	}
	assert.True(t, hasSystemInstruction, "Should have system instruction message")

	// Check that we have task summaries
	hasSummaries := false
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleSystem && contains(msg.Content, "Recent Task Context") {
			hasSummaries = true
			assert.Contains(t, msg.Content, "Summary of first task")
			assert.Contains(t, msg.Content, "Summary of second task")
			break
		}
	}
	assert.True(t, hasSummaries, "Should have task summaries")

	// Check that we have current task state
	hasTaskState := false
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleSystem && contains(msg.Content, "Current Task") {
			hasTaskState = true
			assert.Contains(t, msg.Content, "Current task goal")
			break
		}
	}
	assert.True(t, hasTaskState, "Should have current task state")

	// Check that user message is the original message (not the generated goal)
	lastMsg := messages[len(messages)-1]
	assert.Equal(t, chat.MessageRoleUser, lastMsg.Role)
	assert.Equal(t, activeTask.OriginalMessage, lastMsg.Content)
}

func TestTaskPromptBuilder_NoHistoryIncluded(t *testing.T) {
	// Create a session with chat history
	sess := session.New()

	// Add user message
	sess.AddMessage(session.UserMessage("Hello, can you help me?"))

	// Add assistant message
	prov := &mockProvider{id: "test/mock-model"}
	testAgent := agent.New("test", "You are a test agent", agent.WithModel(prov))
	sess.AddMessage(session.NewAgentMessage(testAgent, &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "Sure, I can help you!",
	}))

	// Add another user message and start task
	sess.AddMessage(session.UserMessage("Please do this task"))

	// Start the task
	sess.StartTask("Please do this task", "Please do this task")

	builder := NewTaskPromptBuilder(sess, testAgent)
	messages := builder.BuildMessages(nil)

	// Verify that old chat history is NOT included (only task goal as user message)
	userMsgCount := 0
	assistantMsgCount := 0
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleUser {
			userMsgCount++
		}
		if msg.Role == chat.MessageRoleAssistant {
			assistantMsgCount++
		}
	}

	// Should only have ONE user message (the current task goal)
	assert.Equal(t, 1, userMsgCount, "Should only have the task goal as user message, not full history")
	// Should have NO assistant messages (since we're starting fresh)
	assert.Equal(t, 0, assistantMsgCount, "Should not include previous assistant messages")
}

func TestTaskPromptBuilder_WithCurrentRunMessages(t *testing.T) {
	sess := session.New()
	sess.StartTask("Test task goal", "Test task goal")

	prov := &mockProvider{id: "test/mock-model"}
	testAgent := agent.New("test", "You are a test agent", agent.WithModel(prov))

	// Simulate current run messages (assistant response + tool call + tool result)
	currentRunMessages := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me check that for you.",
		},
		{
			Role:       chat.MessageRoleTool,
			ToolCallID: "call_123",
			Content:    "Tool result here",
		},
	}

	builder := NewTaskPromptBuilder(sess, testAgent)
	messages := builder.BuildMessages(currentRunMessages)

	// Verify current run messages are included
	found := 0
	for _, msg := range messages {
		if msg.Role == chat.MessageRoleAssistant && msg.Content == "Let me check that for you." {
			found++
		}
		if msg.Role == chat.MessageRoleTool && msg.Content == "Tool result here" {
			found++
		}
	}
	assert.Equal(t, 2, found, "Current run messages should be included")
}

func TestTask_Lifecycle(t *testing.T) {
	task := session.NewTask("Test goal", "Test goal")

	// Test initial state
	assert.True(t, task.IsActive())
	assert.False(t, task.IsWaiting())
	assert.False(t, task.IsCompleted())

	// Test update state
	task.UpdateState("Working on step 1")
	assert.Equal(t, "Working on step 1", task.State)

	// Test mark waiting
	task.MarkWaiting("What file should I edit?")
	assert.False(t, task.IsActive())
	assert.True(t, task.IsWaiting())
	assert.Equal(t, "What file should I edit?", task.WaitingQuestion)

	// Test resume
	task.Resume()
	assert.True(t, task.IsActive())
	assert.Empty(t, task.WaitingQuestion)

	// Test mark completed
	task.MarkCompleted("Here is the result", "Completed the task successfully")
	assert.False(t, task.IsActive())
	assert.True(t, task.IsCompleted())
	assert.Equal(t, "Here is the result", task.FinalResponse)
	assert.Equal(t, "Completed the task successfully", task.Summary)
	assert.NotNil(t, task.CompletedAt)
}

func TestSession_TaskManagement(t *testing.T) {
	sess := session.New()

	// Initially no active task
	assert.Nil(t, sess.ActiveTask())
	assert.Empty(t, sess.CompletedTasks())

	// Start a task
	task1 := sess.StartTask("First goal", "First goal")
	assert.Equal(t, task1, sess.ActiveTask())
	assert.Len(t, sess.Tasks, 1)

	// Complete the task
	task1.MarkCompleted("Response 1", "Summary 1")
	sess.ClearActiveTask()
	assert.Nil(t, sess.ActiveTask())
	assert.Len(t, sess.CompletedTasks(), 1)

	// Start another task
	task2 := sess.StartTask("Second goal", "Second goal")
	assert.Equal(t, task2, sess.ActiveTask())
	assert.Len(t, sess.Tasks, 2)
}

func TestSession_RecentTaskSummaries(t *testing.T) {
	sess := session.New()
	sess.TaskSummaryCount = 2 // Only keep 2 recent summaries

	// Add 4 completed tasks
	for i := 1; i <= 4; i++ {
		goalStr := "Goal " + string(rune('0'+i))
		task := sess.StartTask(goalStr, goalStr)
		task.MarkCompleted("Response", "Summary "+string(rune('0'+i)))
		sess.ClearActiveTask()
	}

	summaries := sess.RecentTaskSummaries()
	assert.Len(t, summaries, 2, "Should only return 2 most recent summaries")
	assert.Equal(t, "Summary 3", summaries[0])
	assert.Equal(t, "Summary 4", summaries[1])
}

func TestTaskMarkdownBuilder(t *testing.T) {
	task := session.NewTask("Write a function to calculate fibonacci numbers", "Write a function to calculate fibonacci numbers")
	task.UpdateState("Created function, added tests")
	task.MarkCompleted(
		"I've created the fibonacci function with proper error handling.",
		"Created fibonacci function in utils.go with unit tests.",
	)

	content := buildTaskMarkdown(task)

	// Verify content structure
	assert.Contains(t, content, "# Task:")
	assert.Contains(t, content, "fibonacci")
	assert.Contains(t, content, "## Metadata")
	assert.Contains(t, content, task.ID)
	assert.Contains(t, content, "completed")
	assert.Contains(t, content, "## Goal")
	assert.Contains(t, content, "Write a function to calculate fibonacci numbers")
	assert.Contains(t, content, "## Final State")
	assert.Contains(t, content, "Created function, added tests")
	assert.Contains(t, content, "## Final Response")
	assert.Contains(t, content, "fibonacci function with proper error handling")
	assert.Contains(t, content, "## Summary")
	assert.Contains(t, content, "Created fibonacci function in utils.go")
}

func TestWriteTaskMarkdown(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()

	// Override home directory for the test
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", tempDir)
	defer os.Setenv("HOME", oldHome)

	// Create a runtime and task
	prov := &mockProvider{id: "test/mock-model"}
	testAgent := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(testAgent))
	rt, err := New(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	task := session.NewTask("Test task", "Test task")
	task.MarkCompleted("Test response", "Test summary")

	// Write the markdown
	err = rt.writeTaskMarkdown("test-session-id", task)
	require.NoError(t, err)

	// Verify file was created
	expectedPath := filepath.Join(tempDir, ".cagent", "sessions", "test-session-id", "tasks", task.ID+".md")
	_, err = os.Stat(expectedPath)
	require.NoError(t, err, "Task markdown file should exist")

	// Read and verify content
	content, err := os.ReadFile(expectedPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "Test task")
	assert.Contains(t, string(content), "Test response")
	assert.Contains(t, string(content), "Test summary")
}

func TestTaskControlToolDefinitions(t *testing.T) {
	tc := builtin.NewTaskControlTool()
	tools, err := tc.Tools(nil)
	require.NoError(t, err)

	// Should have 3 tools
	require.Len(t, tools, 3)

	// Verify tool names
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
		assert.Equal(t, "task_control", tool.Category)
	}

	assert.True(t, names[builtin.ToolNameTaskUpdateState])
	assert.True(t, names[builtin.ToolNameTaskWaitingOnUser])
	assert.True(t, names[builtin.ToolNameTaskComplete])
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		expected string
	}{
		{30 * time.Second, "30 seconds"},
		{90 * time.Second, "1 minutes 30 seconds"},
		{5 * time.Minute, "5 minutes"},
		{65 * time.Minute, "1 hours 5 minutes"},
		{2 * time.Hour, "2 hours"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			result := formatDuration(tc.duration)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestTruncateForTitle(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"Short", 10, "Short"},
		{"This is a very long title that needs truncation", 20, "This is a very lo..."},
		{"Multi\nline\ntext", 20, "Multi line text"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			result := truncateForTitle(tc.input, tc.maxLen)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
