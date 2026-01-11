package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/skills"
)

// TaskPromptBuilder builds prompts for kruntime task-based context mode.
// Unlike the standard GetMessages, it does NOT include full chat history.
// Instead, it includes:
// 1. System: agent identity + instructions
// 2. System: dynamic prompt parts (prompt files, environment info)
// 3. System: last-N completed task summaries
// 4. System: current active task state (goal + state + waiting question if any)
// 5. User: current user message (from task goal or resumed with new message)
// 6. Current run messages (assistant/tool messages from this run only)
type TaskPromptBuilder struct {
	sess  *session.Session
	agent *agent.Agent
}

// NewTaskPromptBuilder creates a new task prompt builder
func NewTaskPromptBuilder(sess *session.Session, agent *agent.Agent) *TaskPromptBuilder {
	return &TaskPromptBuilder{
		sess:  sess,
		agent: agent,
	}
}

// BuildMessages builds the message list for a kruntime model call.
// currentRunMessages are the assistant/tool messages accumulated during the current autonomous loop.
func (b *TaskPromptBuilder) BuildMessages(currentRunMessages []chat.Message) []chat.Message {
	var messages []chat.Message

	// 1. Add system messages (agent identity, instructions, dynamic parts)
	messages = append(messages, b.buildSystemMessages()...)

	// 2. Add task summaries from recent completed tasks
	messages = append(messages, b.buildTaskSummaryMessages()...)

	// 3. Add current task state
	messages = append(messages, b.buildCurrentTaskMessages()...)

	// 4. Add the user message (task goal or resumed question)
	messages = append(messages, b.buildUserMessage())

	// 5. Add current run messages (assistant/tool messages from this run)
	messages = append(messages, currentRunMessages...)

	return messages
}

// buildSystemMessages creates the system messages with agent instructions and dynamic parts
func (b *TaskPromptBuilder) buildSystemMessages() []chat.Message {
	var messages []chat.Message

	a := b.agent

	// Add handoff prompt if there are handoffs (same as original)
	if handoffs := a.Handoffs(); len(handoffs) > 0 {
		var agentsInfo string
		var validAgentIDs []string
		for _, handoffAgent := range handoffs {
			agentsInfo += "Name: " + handoffAgent.Name() + " | Description: " + handoffAgent.Description() + "\n"
			validAgentIDs = append(validAgentIDs, handoffAgent.Name())
		}

		handoffPrompt := "You are part of a multi-agent team. Your goal is to answer the user query in the most helpful way possible.\n\n" +
			"Available agents in your team:\n" + agentsInfo + "\n" +
			"You can hand off the conversation to any of these agents at any time by using the `handoff` function with their ID. " +
			"The valid agent IDs are: " + strings.Join(validAgentIDs, ", ") + ".\n\n" +
			"When to hand off:\n" +
			"- If another agent's description indicates they are better suited for the current task or question\n" +
			"- If the user explicitly asks for a specific agent\n" +
			"- If you need specialized capabilities that another agent provides\n\n" +
			"If you are the best agent to handle the current request based on your capabilities, respond directly. " +
			"When handing off to another agent, only handoff without talking about the handoff."

		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleSystem,
			Content: handoffPrompt,
		})
	}

	// Build main instruction content
	content := a.Instruction()

	if a.AddDate() {
		content += "\n\n" + "Today's date: " + time.Now().Format("2006-01-02")
	}

	// Add environment info and prompt files
	wd := b.sess.WorkingDir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			slog.Error("getting current working directory for environment info", "error", err)
		}
	}
	if wd != "" {
		if a.AddEnvironmentInfo() {
			content += "\n\n" + session.GetEnvironmentInfo(wd)
		}

		for _, prompt := range a.AddPromptFiles() {
			additionalPrompt, err := session.ReadPromptFile(wd, prompt)
			if err != nil {
				slog.Error("reading prompt file", "file", prompt, "error", err)
				continue
			}

			if additionalPrompt != "" {
				content += "\n\n" + additionalPrompt
			}
		}
	}

	// Add skills section if enabled
	if a.SkillsEnabled() {
		loadedSkills := skills.Load()
		if len(loadedSkills) > 0 {
			content += skills.BuildSkillsPrompt(loadedSkills)
		}
	}

	messages = append(messages, chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: content,
	})

	// Add tool instructions
	for _, tool := range a.ToolSets() {
		if tool.Instructions() != "" {
			messages = append(messages, chat.Message{
				Role:    chat.MessageRoleSystem,
				Content: tool.Instructions(),
			})
		}
	}

	return messages
}

// buildTaskSummaryMessages creates system messages with recent task summaries
func (b *TaskPromptBuilder) buildTaskSummaryMessages() []chat.Message {
	summaries := b.sess.RecentTaskSummaries()
	if len(summaries) == 0 {
		return nil
	}

	var summaryContent strings.Builder
	summaryContent.WriteString("## Recent Task Context\n\n")
	summaryContent.WriteString("Here are summaries of recently completed tasks for context:\n\n")

	for i, summary := range summaries {
		summaryContent.WriteString(fmt.Sprintf("**Task %d:**\n%s\n\n", i+1, summary))
	}

	return []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: summaryContent.String(),
		},
	}
}

// buildCurrentTaskMessages creates system messages describing the current task state
func (b *TaskPromptBuilder) buildCurrentTaskMessages() []chat.Message {
	task := b.sess.ActiveTask()
	if task == nil {
		return nil
	}

	var taskContent strings.Builder
	taskContent.WriteString("## Current Task\n\n")
	taskContent.WriteString(fmt.Sprintf("**Goal:** %s\n\n", task.Goal))

	if task.State != "" {
		taskContent.WriteString(fmt.Sprintf("**Current State:** %s\n\n", task.State))
	}

	if task.WaitingQuestion != "" && task.Status == session.TaskStatusWaiting {
		taskContent.WriteString(fmt.Sprintf("**Previously asked user:** %s\n\n", task.WaitingQuestion))
		taskContent.WriteString("The user has now responded. Continue working on the task.\n")
	}

	return []chat.Message{
		{
			Role:    chat.MessageRoleSystem,
			Content: taskContent.String(),
		},
	}
}

// buildUserMessage creates the user message based on task state
func (b *TaskPromptBuilder) buildUserMessage() chat.Message {
	task := b.sess.ActiveTask()

	if task == nil {
		// This shouldn't happen in normal operation, but handle gracefully
		return chat.Message{
			Role:    chat.MessageRoleUser,
			Content: "Please help me.",
		}
	}

	// If task was waiting and is now resumed, the user has provided additional input
	// which should be in the last user message in Messages
	if task.Status == session.TaskStatusActive && task.WaitingQuestion != "" {
		// Find the last user message (the response to the waiting question)
		for i := len(b.sess.Messages) - 1; i >= 0; i-- {
			if item := b.sess.Messages[i]; item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
				return chat.Message{
					Role:    chat.MessageRoleUser,
					Content: item.Message.Message.Content,
				}
			}
		}
	}

	// Use the original user message (what the user actually asked) as the user message.
	// The Goal is an LLM-generated concise version shown in the task context.
	userContent := task.OriginalMessage
	if userContent == "" {
		// Fall back to goal if original message not available (backward compatibility)
		userContent = task.Goal
	}

	return chat.Message{
		Role:    chat.MessageRoleUser,
		Content: userContent,
	}
}
