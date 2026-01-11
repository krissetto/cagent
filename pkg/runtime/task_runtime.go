package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/telemetry"
	"github.com/docker/cagent/pkg/tools"
	"github.com/docker/cagent/pkg/tools/builtin"
)

// TaskControlResult represents the result of a task control tool call
type TaskControlResult struct {
	// Type is the type of task control (update_state, waiting_on_user, complete)
	Type string

	// State is the updated task state (for update_state)
	State string

	// Question is the question for the user (for waiting_on_user)
	Question string

	// FinalResponse is the final response for the user (for complete)
	FinalResponse string

	// Summary is the task summary (for complete)
	Summary string
}

// isTaskControlTool returns true if the tool name is a task control tool
func isTaskControlTool(name string) bool {
	switch name {
	case builtin.ToolNameTaskUpdateState, builtin.ToolNameTaskWaitingOnUser, builtin.ToolNameTaskComplete:
		return true
	}
	return false
}

// runTaskStream is the kruntime-mode implementation of RunStream.
// It uses task-based context management with an autonomous loop.
func (r *LocalRuntime) runTaskStream(ctx context.Context, sess *session.Session, events chan Event, sessionSpan trace.Span) {
	a := r.CurrentAgent()

	// Ensure there's an active task
	task := sess.ActiveTask()
	isNewTask := task == nil
	if isNewTask {
		// Find the user's message
		userMessage := r.findUserGoal(sess)
		if userMessage == "" {
			events <- Error("No user message found to start task")
			return
		}

		// Emit user message event immediately so the UI shows it right away
		events <- UserMessage(userMessage)

		// Generate a concise goal from the user message using the LLM
		// This happens after the user message is shown to avoid UI lag
		goal := r.generateTaskGoal(ctx, a, userMessage)
		if goal == "" {
			// Fall back to user message if goal generation fails
			goal = userMessage
		}

		task = sess.StartTask(goal, userMessage)
		slog.Debug("Started new task", "task_id", task.ID, "goal", goal, "original_message", userMessage)

		// Emit task started event with the generated goal
		events <- TaskStarted(task.ID, goal, a.Name())
	} else if task.IsWaiting() {
		// Resume a waiting task
		task.Resume()
		slog.Debug("Resumed waiting task", "task_id", task.ID)

		// Find the new user message (response to waiting question)
		if newMsg := r.findUserGoal(sess); newMsg != "" {
			events <- UserMessage(newMsg)
		}
	}

	// Get tools once at the start (filter out task control tools from display)
	agentTools, err := r.getTools(ctx, a, sessionSpan, events)
	if err != nil {
		events <- Error(fmt.Sprintf("failed to get tools: %v", err))
		return
	}

	// Count visible tools (excluding task control tools)
	visibleToolCount := 0
	for _, t := range agentTools {
		if !isTaskControlTool(t.Name) {
			visibleToolCount++
		}
	}

	events <- ToolsetInfo(visibleToolCount, false, r.currentAgent)
	events <- StreamStarted(sess.ID, a.Name())

	// Register default tools (transfer_task, handoff) plus task control tools
	r.registerDefaultTools()
	r.registerTaskControlTools()

	// Create a filtered events channel that hides task control tool events
	filteredEvents := make(chan Event, 128)
	go func() {
		for event := range filteredEvents {
			// Filter out task control tool events
			switch e := event.(type) {
			case *PartialToolCallEvent:
				if isTaskControlTool(e.ToolCall.Function.Name) {
					continue // Skip task control tool events
				}
			case *ToolCallEvent:
				if isTaskControlTool(e.ToolCall.Function.Name) {
					continue
				}
			case *ToolCallConfirmationEvent:
				if isTaskControlTool(e.ToolCall.Function.Name) {
					continue
				}
			case *ToolCallResponseEvent:
				if isTaskControlTool(e.ToolCall.Function.Name) {
					continue
				}
			}
			events <- event
		}
	}()
	defer close(filteredEvents)

	// Track messages for the current run only (not persisted to session in prompt)
	var currentRunMessages []chat.Message

	promptBuilder := NewTaskPromptBuilder(sess, a)

	iteration := 0
	maxIterations := 100 // Safety cap for kruntime mode

	for {
		// Check context cancellation
		if err := ctx.Err(); err != nil {
			slog.Debug("Task runtime context cancelled", "agent", a.Name(), "task_id", task.ID)
			return
		}

		// Check iteration limit
		if iteration >= maxIterations {
			slog.Warn("Task runtime reached max iterations", "agent", a.Name(), "task_id", task.ID, "max", maxIterations)
			events <- Error(fmt.Sprintf("Task reached maximum iterations (%d). Please try breaking the task into smaller steps.", maxIterations))
			return
		}
		iteration++

		slog.Debug("Task runtime iteration", "agent", a.Name(), "task_id", task.ID, "iteration", iteration)

		// Wrap context with active task so usage can be attributed across the call chain
		// (including transfer_task sub-sessions)
		taskCtx := session.WithActiveTask(ctx, task)

		streamCtx, streamSpan := r.startSpan(taskCtx, "runtime.task_stream", trace.WithAttributes(
			attribute.String("agent", a.Name()),
			attribute.String("session.id", sess.ID),
			attribute.String("task.id", task.ID),
			attribute.Int("iteration", iteration),
		))

		model := a.Model()
		modelID := model.ID()

		// Build messages using task prompt builder
		messages := promptBuilder.BuildMessages(currentRunMessages)
		slog.Debug("Built task prompt", "agent", a.Name(), "message_count", len(messages), "current_run_messages", len(currentRunMessages))

		// Create chat completion stream
		stream, err := model.CreateChatCompletionStream(streamCtx, messages, agentTools)
		if err != nil {
			streamSpan.RecordError(err)
			streamSpan.SetStatus(codes.Error, "creating chat completion")
			slog.Error("Failed to create chat completion stream", "agent", a.Name(), "error", err)
			telemetry.RecordError(ctx, err.Error())
			events <- Error(fmt.Sprintf("creating chat completion: %v", err))
			streamSpan.End()
			return
		}

		// Get model definition for context limit
		m, _ := r.modelsStore.GetModel(streamCtx, modelID)

		// Handle the stream (use filteredEvents to hide task control tools)
		// streamCtx carries the active task for usage attribution
		res, err := r.handleStream(streamCtx, stream, a, agentTools, sess, m, filteredEvents)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Debug("Task stream canceled by context", "agent", a.Name(), "task_id", task.ID)
				streamSpan.End()
				return
			}
			streamSpan.RecordError(err)
			streamSpan.SetStatus(codes.Error, "error handling stream")
			slog.Error("Error handling task stream", "agent", a.Name(), "error", err)
			telemetry.RecordError(ctx, err.Error())
			events <- Error(err.Error())
			streamSpan.End()
			return
		}

		streamSpan.SetAttributes(
			attribute.Int("tool.calls", len(res.Calls)),
			attribute.Int("content.length", len(res.Content)),
			attribute.Bool("stopped", res.Stopped),
		)
		streamSpan.End()

		// Check if this response contains only task control tool calls
		hasOnlyTaskControlCalls := len(res.Calls) > 0
		hasRegularToolCalls := false
		for _, call := range res.Calls {
			if !isTaskControlTool(call.Function.Name) {
				hasOnlyTaskControlCalls = false
				hasRegularToolCalls = true
			}
		}

		// Build assistant message for non-task-control operations
		// Skip adding raw messages when only task control tools are called
		// (we'll add a clean final response instead)
		if !hasOnlyTaskControlCalls && (strings.TrimSpace(res.Content) != "" || hasRegularToolCalls) {
			// Build tool definitions (only for non-task-control tools)
			var toolDefs []tools.Tool
			var filteredCalls []tools.ToolCall
			if len(res.Calls) > 0 {
				toolMap := make(map[string]tools.Tool, len(agentTools))
				for _, t := range agentTools {
					toolMap[t.Name] = t
				}
				for _, call := range res.Calls {
					if !isTaskControlTool(call.Function.Name) {
						if def, ok := toolMap[call.Function.Name]; ok {
							toolDefs = append(toolDefs, def)
						}
						filteredCalls = append(filteredCalls, call)
					}
				}
			}

			// Calculate cost
			var messageCost float64
			if res.Usage != nil && m != nil && m.Cost != nil {
				messageCost = (float64(res.Usage.InputTokens)*m.Cost.Input +
					float64(res.Usage.OutputTokens)*m.Cost.Output +
					float64(res.Usage.CachedInputTokens)*m.Cost.CacheRead +
					float64(res.Usage.CacheWriteTokens)*m.Cost.CacheWrite) / 1e6
			}

			messageModel := modelID
			if res.ActualModel != "" {
				messageModel = res.ActualModel
			}

			assistantMessage := chat.Message{
				Role:              chat.MessageRoleAssistant,
				Content:           res.Content,
				ReasoningContent:  res.ReasoningContent,
				ThinkingSignature: res.ThinkingSignature,
				ThoughtSignature:  res.ThoughtSignature,
				ToolCalls:         filteredCalls, // Only include non-task-control tool calls
				ToolDefinitions:   toolDefs,
				CreatedAt:         time.Now().Format(time.RFC3339),
				Usage:             res.Usage,
				Model:             messageModel,
				Cost:              messageCost,
			}

			// Add to session (for persistence and display)
			sess.AddMessage(session.NewAgentMessage(a, &assistantMessage))
			_ = r.sessionStore.UpdateSession(taskCtx, sess)

			// Add to current run messages (for next prompt in this task)
			currentRunMessages = append(currentRunMessages, assistantMessage)
		}

		// Still need to track full tool calls for current run messages
		if hasOnlyTaskControlCalls {
			// For task control only calls, add the raw response to current run for LLM context
			assistantMessage := chat.Message{
				Role:      chat.MessageRoleAssistant,
				Content:   res.Content,
				ToolCalls: res.Calls,
				CreatedAt: time.Now().Format(time.RFC3339),
			}
			currentRunMessages = append(currentRunMessages, assistantMessage)
		}

		var contextLimit int64
		if m != nil {
			contextLimit = int64(m.Limit.Context)
		}
		events <- TokenUsage(sess.ID, r.currentAgent, sess.InputTokens, sess.OutputTokens, sess.InputTokens+sess.OutputTokens, contextLimit, sess.Cost)

		// Process tool calls and check for task control tools
		// Use taskCtx so transfer_task sub-sessions inherit the active task for usage attribution
		taskControlResult, toolResults := r.processTaskToolCalls(taskCtx, sess, res.Calls, agentTools, events)

		// Add tool results to current run messages
		currentRunMessages = append(currentRunMessages, toolResults...)

		// Handle task control results
		if taskControlResult != nil {
			switch taskControlResult.Type {
			case builtin.ToolNameTaskUpdateState:
				// Update task state and continue
				task.UpdateState(taskControlResult.State)
				_ = r.sessionStore.UpdateSession(taskCtx, sess)
				slog.Debug("Task state updated", "task_id", task.ID, "state", taskControlResult.State)

				// Emit state updated event
				events <- TaskStateUpdated(task.ID, taskControlResult.State, a.Name())
				continue

			case builtin.ToolNameTaskWaitingOnUser:
				// Mark task as waiting and stop
				task.MarkWaiting(taskControlResult.Question)
				_ = r.sessionStore.UpdateSession(taskCtx, sess)
				slog.Debug("Task waiting on user", "task_id", task.ID, "question", taskControlResult.Question)

				// Emit the question as the assistant's response
				events <- AgentChoice(a.Name(), taskControlResult.Question)

				// Emit task waiting event
				events <- TaskWaiting(task.ID, taskControlResult.Question, a.Name())
				return

			case builtin.ToolNameTaskComplete:
				// Only emit final response if the model didn't already output content
				// (some models output content AND call the tool, resulting in duplicates)
				if strings.TrimSpace(res.Content) == "" {
					events <- AgentChoice(a.Name(), taskControlResult.FinalResponse)
				}

				// Mark task as complete, write markdown, and stop
				task.MarkCompleted(taskControlResult.FinalResponse, taskControlResult.Summary)
				sess.ClearActiveTask()

				// Add the final response as an assistant message in session for history
				// Use the final_response from the tool, not the streamed content
				finalMsg := &chat.Message{
					Role:      chat.MessageRoleAssistant,
					Content:   taskControlResult.FinalResponse,
					CreatedAt: time.Now().Format(time.RFC3339),
				}
				sess.AddMessage(session.NewAgentMessage(a, finalMsg))
				_ = r.sessionStore.UpdateSession(taskCtx, sess)

				// Write task markdown file
				if err := r.writeTaskMarkdown(sess.ID, task); err != nil {
					slog.Warn("Failed to write task markdown", "task_id", task.ID, "error", err)
				}

				// Emit task completed event
				events <- TaskCompleted(task.ID, taskControlResult.Summary, a.Name())

				slog.Debug("Task completed", "task_id", task.ID)
				return
			}
		}

		// If model stopped but no task control tool was called, inject a continue message
		if res.Stopped && len(res.Calls) == 0 {
			slog.Debug("Model stopped without task control, injecting continue prompt", "task_id", task.ID)

			// Add an implicit continue message to current run
			continueMsg := chat.Message{
				Role:    chat.MessageRoleUser,
				Content: "Continue working on the task. Remember to call task_complete when done, or task_waiting_on_user if you need more information from me.",
			}
			currentRunMessages = append(currentRunMessages, continueMsg)
		}
	}
}

// findUserGoal finds the goal from the last user message in the session
func (r *LocalRuntime) findUserGoal(sess *session.Session) string {
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		item := sess.Messages[i]
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			return item.Message.Message.Content
		}
	}
	return ""
}

// registerTaskControlTools registers handlers for task control tools
func (r *LocalRuntime) registerTaskControlTools() {
	tc := builtin.NewTaskControlTool()
	tcTools, _ := tc.Tools(context.TODO())

	// Task control tools don't have actual handlers - they're processed specially
	// We just need to register them so they're recognized
	for _, t := range tcTools {
		r.toolMap[t.Name] = ToolHandler{tool: t, handler: nil}
	}

	slog.Debug("Registered task control tools", "count", len(tcTools))
}

// processTaskToolCalls processes tool calls and returns task control results and tool result messages.
// It separates task control tools from regular tools.
func (r *LocalRuntime) processTaskToolCalls(
	ctx context.Context,
	sess *session.Session,
	calls []tools.ToolCall,
	agentTools []tools.Tool,
	events chan Event,
) (*TaskControlResult, []chat.Message) {
	var toolResults []chat.Message
	var taskControlResult *TaskControlResult

	for _, call := range calls {
		// Check if this is a task control tool
		switch call.Function.Name {
		case builtin.ToolNameTaskUpdateState:
			var args builtin.TaskUpdateStateArgs
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				slog.Warn("Failed to parse task_update_state args", "error", err)
				continue
			}
			taskControlResult = &TaskControlResult{
				Type:  builtin.ToolNameTaskUpdateState,
				State: args.State,
			}
			// Add a tool result message
			toolResults = append(toolResults, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: call.ID,
				Content:    "Task state updated.",
			})

		case builtin.ToolNameTaskWaitingOnUser:
			var args builtin.TaskWaitingOnUserArgs
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				slog.Warn("Failed to parse task_waiting_on_user args", "error", err)
				continue
			}
			taskControlResult = &TaskControlResult{
				Type:     builtin.ToolNameTaskWaitingOnUser,
				Question: args.Question,
			}
			// Add a tool result message
			toolResults = append(toolResults, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: call.ID,
				Content:    "Waiting for user response.",
			})

		case builtin.ToolNameTaskComplete:
			var args builtin.TaskCompleteArgs
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				slog.Warn("Failed to parse task_complete args", "error", err)
				continue
			}
			taskControlResult = &TaskControlResult{
				Type:          builtin.ToolNameTaskComplete,
				FinalResponse: args.FinalResponse,
				Summary:       args.Summary,
			}
			// Add a tool result message
			toolResults = append(toolResults, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: call.ID,
				Content:    "Task marked as complete.",
			})

		default:
			// Regular tool - process normally
			r.processToolCalls(ctx, sess, []tools.ToolCall{call}, agentTools, events)

			// Get the tool result from the session (it was just added)
			if len(sess.Messages) > 0 {
				lastItem := sess.Messages[len(sess.Messages)-1]
				if lastItem.Message != nil && lastItem.Message.Message.Role == chat.MessageRoleTool {
					toolResults = append(toolResults, lastItem.Message.Message)
				}
			}
		}
	}

	return taskControlResult, toolResults
}

// generateTaskGoal generates a concise goal description from the user's message.
// This makes a quick LLM call to create a clear, actionable goal statement.
func (r *LocalRuntime) generateTaskGoal(ctx context.Context, a *agent.Agent, userMessage string) string {
	model := a.Model()
	if model == nil {
		slog.Debug("No model available for goal generation, using user message as goal")
		return ""
	}

	// Create a simple prompt to generate a concise goal
	messages := []chat.Message{
		{
			Role: chat.MessageRoleSystem,
			Content: `You are a task goal generator. Given a user's message, generate a clear, concise goal statement (max 80 chars).
The goal should be an action-oriented summary of what the user wants accomplished.
Respond with ONLY the goal statement, nothing else. No quotes, no explanation.

Examples:
User: "Hey can you help me fix the bug in login.go where users can't authenticate with expired tokens?"
Goal: Fix expired token authentication bug in login.go

User: "I need you to write a Python script that downloads all images from a webpage"
Goal: Create Python script to download images from webpage

User: "What's the weather like today?"
Goal: Check current weather conditions`,
		},
		{
			Role:    chat.MessageRoleUser,
			Content: userMessage,
		},
	}

	// Make a quick completion call (no tools needed)
	stream, err := model.CreateChatCompletionStream(ctx, messages, nil)
	if err != nil {
		slog.Debug("Failed to create goal generation stream", "error", err)
		return ""
	}

	var goal strings.Builder
	for {
		response, err := stream.Recv()
		if err != nil {
			break
		}
		if len(response.Choices) > 0 {
			goal.WriteString(response.Choices[0].Delta.Content)
		}
	}

	result := strings.TrimSpace(goal.String())
	// Truncate if too long
	if len(result) > 100 {
		result = result[:97] + "..."
	}

	slog.Debug("Generated task goal", "original", userMessage, "goal", result)
	return result
}
