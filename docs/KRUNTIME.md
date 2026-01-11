# Task-Based Context Runtime (kruntime)

The task-based context runtime (`--kruntime`) is an experimental feature that fundamentally changes how cagent manages conversation context. Instead of sending the full chat history with every request, it uses a **task-oriented approach** that minimizes token usage while maintaining full conversation persistence.

## Motivation

Traditional LLM agent loops send the entire conversation history with each request. This has several drawbacks:

1. **Token waste**: Previous tool calls, reasoning, and intermediate steps consume context window space
2. **Cost accumulation**: Every message costs more as the conversation grows
3. **Model attention degradation**: Long contexts can dilute model focus on the current task
4. **Context limits**: Eventually hitting the model's context window limit

The kruntime solves this by treating each user request as a discrete **task** with its own lifecycle, while preserving full history for display and reference.

## How It Works

### Task Lifecycle

```
┌──────────────────────────────────────────────────────────────┐
│                        User Message                          │
│  "Hey can you fix the bug in login.go where users can't     │
│   authenticate with expired tokens?"                         │
└──────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────┐
│                   Goal Generation (LLM)                      │
│  Generates concise, action-oriented goal from user message  │
└──────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────┐
│                      Task Created                            │
│  ID: abc-123                                                 │
│  Goal: "Fix expired token authentication bug in login.go"   │
│  Original: (full user message preserved)                     │
│  Status: active                                              │
└──────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────┐
│                    Autonomous Loop                           │
│  • Agent reads files, runs tools                             │
│  • Calls task_update_state to save progress                  │
│  • Continues until task_complete or task_waiting_on_user     │
└──────────────────────────────────────────────────────────────┘
                              │
                    ┌─────────┴─────────┐
                    ▼                   ▼
┌─────────────────────────┐  ┌─────────────────────────┐
│    task_complete        │  │  task_waiting_on_user   │
│  • Final response       │  │  • Question for user    │
│  • Summary generated    │  │  • Task pauses          │
│  • Context reset        │  │  • Awaits response      │
└─────────────────────────┘  └─────────────────────────┘
```

### Context Structure

When kruntime is enabled, each model call receives a minimal, focused context:

1. **System prompt**: Agent identity, instructions, and behavior
2. **Dynamic context**: Environment info, AGENTS.md, prompt files
3. **Recent task summaries**: Last 3 completed task summaries (configurable)
4. **Current task state**: LLM-generated goal + progress state (if set)
5. **User message**: The original user message (not the generated goal)
6. **Current run messages**: Only tool calls/results from the current task

**Crucially excluded**: Full chat history, previous task tool calls, old reasoning

### Goal Generation

When a new task is created, the runtime makes a quick LLM call to generate a concise, action-oriented goal from the user's message. This goal is:

- **Max 80 characters**: Keeps the sidebar and task display clean
- **Action-oriented**: Focuses on what needs to be accomplished
- **Used for display**: Shown in sidebar, cost breakdown, and task markdown
- **Separate from user message**: The full original message is preserved and sent to the agent

Example:
- User: "Hey can you help me fix the bug in login.go where users can't authenticate with expired tokens?"
- Goal: "Fix expired token authentication bug in login.go"

### Task Control Tools

The agent uses three built-in tools to manage task lifecycle:

| Tool | Purpose |
|------|---------|
| `task_update_state` | Save progress during complex tasks. Updates the compact state representation. |
| `task_waiting_on_user` | Pause and ask the user for clarification or input. |
| `task_complete` | Mark task done. Provide final response and summary for future context. |

These tools are:
- **Auto-approved**: No user confirmation required
- **Hidden from UI**: Tool calls don't appear in chat
- **Processed by runtime**: Not sent to external handlers

## Usage

### Enable via CLI

```bash
cagent run config.yaml --kruntime
```

### Enable via API

Pass `kruntime: true` in the runtime configuration when creating sessions.

## Data Persistence

### Session Database

Task data is stored in the session SQLite database with these fields:

- `tasks`: JSON array of all tasks in the session
- `active_task_id`: Currently active task ID
- `task_summary_count`: Number of recent summaries to include (default: 3)

### Markdown Files

Completed tasks are also written as markdown files for easy reference:

```
~/.cagent/sessions/<session_id>/tasks/<task_id>.md
```

Example task markdown:

```markdown
# Task: Fix expired token authentication bug in login.go

## Metadata

- **ID:** `abc-123-def`
- **Status:** completed
- **Created:** 2026-01-11T10:30:00Z
- **Completed:** 2026-01-11T10:35:42Z
- **Duration:** 5 minutes 42 seconds
- **Cost:** $0.04
- **Tokens:** 5.7K in (3 new + 0 cached + 5.7K write) / 287 out

## Goal

Fix expired token authentication bug in login.go

## Original Message

Hey can you help me fix the bug in login.go where users can't 
authenticate with expired tokens?

## Final State

Fixed authentication check in login.go. Updated tests.

## Final Response

I've fixed the login bug. The issue was in the `validateCredentials` 
function which wasn't properly checking for expired tokens...

## Summary

User requested fix for login bug. Found issue in validateCredentials 
function - expired token check was missing. Added proper expiration 
validation and updated unit tests. All tests now pass.
```

## TUI Integration

When kruntime is enabled, the sidebar displays a **Task** section showing:

- **Status indicator**: ▶ Active, ⏸ Waiting for input, ✓ Completed
- **Current goal**: LLM-generated task goal (concise summary)
- **State**: Current progress state (if set via `task_update_state`)
- **Completed count**: Number of completed tasks in the session

The cost dialog (`$` key or `/usage` command) includes a **By Task** section with:

- Per-task cost breakdown
- Token usage (input, cached, cache write, output)
- Task goal for identification

## Architecture

### New Files

| File | Purpose |
|------|---------|
| `pkg/session/task.go` | Task struct and lifecycle methods |
| `pkg/runtime/task_runtime.go` | Autonomous loop implementation |
| `pkg/runtime/task_prompt.go` | Task-focused prompt builder |
| `pkg/runtime/task_markdown.go` | Markdown file generation |
| `pkg/tools/builtin/taskcontrol.go` | Task control tool definitions |
| `pkg/runtime/event.go` | Task lifecycle events (additions) |

### Modified Files

| File | Changes |
|------|---------|
| `pkg/config/runtime.go` | Added `KRuntime` config field |
| `cmd/root/flags.go` | Added `--kruntime` CLI flag |
| `pkg/runtime/runtime.go` | Added kruntime option, conditional routing |
| `pkg/session/session.go` | Added task fields and management methods |
| `pkg/session/store.go` | Persistence for task data |
| `pkg/session/migrations.go` | Database schema for tasks |
| `pkg/teamloader/teamloader.go` | Conditional task control tool injection |
| `pkg/tui/components/sidebar/sidebar.go` | Task display section |
| `pkg/tui/page/chat/runtime_events.go` | Task event handling |
| `pkg/tui/dialog/cost.go` | Per-task cost breakdown display |

### Event Flow

```
Runtime                    TUI Chat Page              Sidebar
   │                            │                        │
   │  TaskStartedEvent          │                        │
   │ ─────────────────────────► │                        │
   │                            │  forwardToSidebar()    │
   │                            │ ─────────────────────► │
   │                            │                        │ Update currentTask
   │                            │                        │
   │  TaskStateUpdatedEvent     │                        │
   │ ─────────────────────────► │ ─────────────────────► │ Update state
   │                            │                        │
   │  TaskCompletedEvent        │                        │
   │ ─────────────────────────► │ ─────────────────────► │ Clear task, ++count
```

## Comparison: Traditional vs kruntime

| Aspect | Traditional | kruntime |
|--------|-------------|----------|
| Context per call | Full history | Task-scoped |
| Token growth | Linear with conversation | Reset per task |
| Model focus | Diluted over time | Fresh each task |
| History display | Same as LLM context | Full (separate from LLM) |
| Task boundaries | Implicit | Explicit |
| Progress tracking | None | Via task state |
| Cost tracking | Session-level only | Per-task breakdown |
| Goal display | Raw user message | LLM-generated summary |

## Limitations

- **Experimental**: API and behavior may change
- **No cross-task memory**: Tasks don't share intermediate state (only summaries)
- **Agent must cooperate**: Requires agent to call task control tools properly
- **Summary quality**: Depends on agent-generated summaries

## Per-Task Cost Tracking

Each task tracks its own token usage and cost:

- **Input tokens**: New tokens sent to the model
- **Cached tokens**: Tokens read from prompt cache
- **Cache write tokens**: Tokens written to prompt cache
- **Output tokens**: Tokens generated by the model
- **Cost**: Total cost for the task

View per-task costs in the cost dialog (press `$` or use `/usage`). The "By Task" section shows a detailed breakdown for each task.

## Future Enhancements

- Task history browser in TUI
- Cross-task state sharing options
- Configurable summary generation
- Task templates and workflows
