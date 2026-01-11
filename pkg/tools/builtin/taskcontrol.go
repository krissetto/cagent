package builtin

import (
	"context"

	"github.com/docker/cagent/pkg/tools"
)

// Task control tool names (used by kruntime)
const (
	ToolNameTaskUpdateState   = "task_update_state"
	ToolNameTaskWaitingOnUser = "task_waiting_on_user"
	ToolNameTaskComplete      = "task_complete"
)

// TaskControlTool provides tools for the agent to control task lifecycle in kruntime mode.
// These tools have no handler - they are processed directly by the runtime.
type TaskControlTool struct {
	tools.BaseToolSet
}

// Make sure TaskControlTool implements the ToolSet Interface
var _ tools.ToolSet = (*TaskControlTool)(nil)

// TaskUpdateStateArgs are the arguments for the task_update_state tool
type TaskUpdateStateArgs struct {
	State string `json:"state" jsonschema:"A compact representation of current task progress. Include key decisions, completed steps, and remaining work. Keep concise but informative."`
}

// TaskWaitingOnUserArgs are the arguments for the task_waiting_on_user tool
type TaskWaitingOnUserArgs struct {
	Question string `json:"question" jsonschema:"The question or clarification you need from the user before proceeding."`
}

// TaskCompleteArgs are the arguments for the task_complete tool
type TaskCompleteArgs struct {
	FinalResponse string `json:"final_response" jsonschema:"The final response to present to the user. This should be a complete answer or summary of what was accomplished."`
	Summary       string `json:"summary" jsonschema:"A brief summary of this task for future context. Include what was requested, key actions taken, and the outcome. Keep under 200 words."`
}

func NewTaskControlTool() *TaskControlTool {
	return &TaskControlTool{}
}

func (t *TaskControlTool) Instructions() string {
	return `## Task Control

You MUST use these tools to manage task lifecycle. Call these tools directly - do NOT output their names or parameters as text.

1. **task_update_state**: Call periodically to save your progress.

2. **task_waiting_on_user**: Call when you need user input before proceeding.

3. **task_complete**: Call when you have completed the user's request. Put your full response in the final_response parameter - do NOT output it as regular text before calling this tool.

IMPORTANT: When finishing a task, call task_complete with your response as the final_response parameter. Do not output the response as text AND call the tool - only call the tool.`
}

func (t *TaskControlTool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameTaskUpdateState,
			Category:    "task_control",
			Description: "Update the current task state to save progress. Call this periodically during complex tasks to maintain context.",
			Parameters:  tools.MustSchemaFor[TaskUpdateStateArgs](),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Update Task State",
			},
		},
		{
			Name:        ToolNameTaskWaitingOnUser,
			Category:    "task_control",
			Description: "Signal that you need user input or clarification before proceeding. Use this when you cannot complete the task without additional information from the user.",
			Parameters:  tools.MustSchemaFor[TaskWaitingOnUserArgs](),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Waiting on User",
			},
		},
		{
			Name:        ToolNameTaskComplete,
			Category:    "task_control",
			Description: "Signal that the task is complete. Provide a final response for the user and a brief summary for future context. You MUST call this when you have finished addressing the user's request.",
			Parameters:  tools.MustSchemaFor[TaskCompleteArgs](),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Task Complete",
			},
		},
	}, nil
}
