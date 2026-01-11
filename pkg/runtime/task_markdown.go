package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/cagent/pkg/session"
)

// writeTaskMarkdown writes a markdown file for a completed task.
// The file is written to ~/.cagent/sessions/<session_id>/tasks/<task_id>.md
func (r *LocalRuntime) writeTaskMarkdown(sessionID string, task *session.Task) error {
	// Get the base directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}

	tasksDir := filepath.Join(homeDir, ".cagent", "sessions", sessionID, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("creating tasks directory: %w", err)
	}

	taskFile := filepath.Join(tasksDir, task.ID+".md")

	content := buildTaskMarkdown(task)

	if err := os.WriteFile(taskFile, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing task file: %w", err)
	}

	return nil
}

// buildTaskMarkdown creates the markdown content for a task
func buildTaskMarkdown(task *session.Task) string {
	var sb strings.Builder

	sb.WriteString("# Task: ")
	sb.WriteString(truncateForTitle(task.Goal, 80))
	sb.WriteString("\n\n")

	sb.WriteString("## Metadata\n\n")
	sb.WriteString(fmt.Sprintf("- **ID:** `%s`\n", task.ID))
	sb.WriteString(fmt.Sprintf("- **Status:** %s\n", task.Status))
	sb.WriteString(fmt.Sprintf("- **Created:** %s\n", task.CreatedAt.Format(time.RFC3339)))
	if task.CompletedAt != nil {
		sb.WriteString(fmt.Sprintf("- **Completed:** %s\n", task.CompletedAt.Format(time.RFC3339)))
		duration := task.CompletedAt.Sub(task.CreatedAt)
		sb.WriteString(fmt.Sprintf("- **Duration:** %s\n", formatDuration(duration)))
	}
	// Include token usage and cost if available
	totalTokens := task.TotalTokens()
	if totalTokens > 0 || task.Cost > 0 {
		sb.WriteString(fmt.Sprintf("- **Tokens:** %s (input: %s, output: %s)\n",
			formatTokenCount(totalTokens),
			formatTokenCount(task.TotalInputTokens()),
			formatTokenCount(task.OutputTokens)))
		sb.WriteString(fmt.Sprintf("- **Cost:** $%.4f\n", task.Cost))
	}
	sb.WriteString("\n")

	sb.WriteString("## Goal\n\n")
	sb.WriteString(task.Goal)
	sb.WriteString("\n\n")

	if task.State != "" {
		sb.WriteString("## Final State\n\n")
		sb.WriteString(task.State)
		sb.WriteString("\n\n")
	}

	if task.FinalResponse != "" {
		sb.WriteString("## Final Response\n\n")
		sb.WriteString(task.FinalResponse)
		sb.WriteString("\n\n")
	}

	if task.Summary != "" {
		sb.WriteString("## Summary\n\n")
		sb.WriteString(task.Summary)
		sb.WriteString("\n")
	}

	return sb.String()
}

// truncateForTitle truncates a string for use in a title
func truncateForTitle(s string, maxLen int) string {
	// Replace newlines with spaces
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")

	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f seconds", d.Seconds())
	}
	if d < time.Hour {
		mins := int(d.Minutes())
		secs := int(d.Seconds()) % 60
		if secs == 0 {
			return fmt.Sprintf("%d minutes", mins)
		}
		return fmt.Sprintf("%d minutes %d seconds", mins, secs)
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins == 0 {
		return fmt.Sprintf("%d hours", hours)
	}
	return fmt.Sprintf("%d hours %d minutes", hours, mins)
}

// formatTokenCount formats a token count with K/M suffixes for readability
func formatTokenCount(count int64) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprintf("%d", count)
	}
}
