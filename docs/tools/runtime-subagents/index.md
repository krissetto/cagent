---
title: "Runtime-managed Subagents Tool"
description: "Start, message, inspect, and stop durable runtime-managed child sessions."
permalink: /tools/runtime-subagents/
---

# Runtime-managed Subagents Tool

_Start, message, inspect, and stop durable runtime-managed child sessions._

## Overview

`subagents` configuration lets an agent create sub-agents as runtime-managed child sessions. Child sessions run independently, emit durable public runtime events, and remain tied to the parent session tree. This is useful for scoped parallel work where the parent should wait for runtime updates instead of polling or duplicating the child’s work.

Unlike the older `background_agents` toolset, runtime-managed subagents use session topology directly: `subagent_start` creates a child session linked to the parent with `AddSubSession`, and lifecycle events are replayable from the session store.

## Available Tools

| Tool                  | Description |
| --------------------- | ----------- |
| `subagent_start`      | Start a runtime-managed child session for an allowed sub-agent. |
| `subagent_send`       | Queue a follow-up or steer message to a running child. Empty or whitespace-only messages are rejected. |
| `subagent_inspect`    | Inspect the latest, recent, or full child transcript and status. |
| `subagent_list`       | List runtime-managed subagents in the current session tree. |
| `subagent_stop`       | Immediately cancel a running child. |
| `subagent_finalize`   | Ask a running child to finish cleanly at its next safe point. |

## Configuration

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-0
    subagents:
      - agent: researcher
        description: Research specialist

  researcher:
    model: openai/gpt-4o
    instruction: Investigate the assigned task and return concise findings.
```

No toolset-specific options are required. The caller lists allowed managed children in `subagents`; docker-agent enables the managed-subagent tools automatically for that agent.

## Lifecycle and replay behavior

Each managed child emits durable `subagent_lifecycle` events:

- `running` when the child session starts.
- `completed` when the child runtime finishes normally.
- `failed` when the child runtime emits an error event; the error is available in `subagent_list`, `subagent_inspect`, and the durable lifecycle payload.
- `stopped` when cancellation is requested. Stop and finalize requests mark the live status immediately; final drain preserves failure status if a child errors while shutting down.

Runtime-managed child sessions are linked into the parent session tree. Their transcript remains in the child session, while the parent stores a subsession item reference and can replay public runtime events by root or child session ID.

See [`examples/runtime_subagents.yaml`]({{ '/examples/runtime_subagents.yaml' | relative_url }}) for a minimal configuration.
