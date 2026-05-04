---
title: "Multi-Agent Systems"
description: "Build teams of specialized agents that collaborate using runtime-managed subagents — a persistent, bidirectional, observable agent tree model."
permalink: /concepts/multi-agent/
---

# Multi-Agent Systems

_Build teams of specialized agents that collaborate using **runtime-managed subagents** — a persistent, bidirectional, observable agent tree model._

## Why Multi-Agent?

Complex tasks benefit from specialization. Instead of one monolithic agent trying to do everything, you can create a **team** of focused agents:

- A **coordinator** that understands the overall goal and delegates
- A **developer** that writes code with filesystem and shell access
- A **reviewer** that checks code quality
- A **researcher** that searches the web for information

Each agent has its own model, tools, and instructions — optimized for its specific role.

## The docker-agent multi-agent model

docker-agent is converging on a **single** multi-agent model: **runtime-managed subagents**. A parent agent can start subagents that run on their own sessions, in the background, and communicate with their parent bidirectionally — the parent can send follow-up messages, and the child wakes the parent when it has something to report. Subagents can themselves start further subagents, forming a tree.

This replaces three older patterns (`transfer_task` + `sub_agents`, `background_agents`, and `handoffs`). Each one solved a slice of the problem; the runtime-managed subsystem subsumes them into one coherent design.

<div class="callout callout-warning" markdown="1">
<div class="callout-title">⚠️ Do not mix legacy multi-agent features with runtime-managed subagents
</div>
  <p>Runtime-managed subagents are designed to <strong>replace</strong> <code>transfer_task</code>, <code>handoff</code>, and <code>background_agents</code>. Do not combine them on the same agent — the legacy and new surfaces are not intended to coexist, and mixing them produces confusing delegation semantics and inconsistent session trees. Pick one model per agent. See the dedicated page: <a href="{{ '/tools/subagents/' | relative_url }}">Subagents (Runtime-Managed)</a>.</p>

</div>

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ Transition status
</div>
  <p>The runtime-managed subagent tools are now enabled by top-level <code>subagents:</code>. Schema v9 also makes <code>subagents</code> the canonical delegation field, while the legacy <code>sub_agents</code> spelling is still accepted for backward compatibility.</p>
  <p>When an agent opts into runtime-managed subagents, do <strong>not</strong> also configure legacy <code>handoffs:</code> or <code>- type: background_agents</code> on that same agent — config validation rejects those combinations. Legacy <code>transfer_task</code>, <code>handoff</code>, and <code>background_agents</code> still exist for compatibility, but they remain on a deprecation path and should not be mixed with the new subsystem.</p>

</div>

## Core concept: subagents on their own sessions

Every subagent in the runtime-managed model has:

- its **own session** (own message history, own token budget, own events)
- its **own agent configuration** (model, instructions, tools)
- a **persistent background loop** that stays alive between turns
- a **parent inbox** the runtime publishes compact update envelopes to

This gives you:

- **Parallelism** — a parent can start several subagents and let them work concurrently.
- **Bidirectionality** — the parent can `subagent_send` follow-up messages to a running subagent; the subagent can wake the parent without polling.
- **Nesting** — a subagent can start its own subagents, up to a configurable depth.
- **Observability** — any external client can attach to any session in the tree via SSE.
- **Control** — any session can be steered, closed, or stopped individually.

## The canonical subagent tools

The runtime exposes:

| Tool | Purpose |
| --- | --- |
| `subagent_start` | Create a new subagent on its own session |
| `subagent_send` | Send a follow-up or steer-mode message to a live subagent (`mode` is optional; default is follow-up) |
| `subagent_list` | List the subagents owned by the current session |
| `subagent_inspect` | Return the subagent's status + latest assistant response by default, with optional recent/full transcript modes |
| `subagent_finalize` | Graceful shutdown after the next safe point (cascades to descendants) |
| `subagent_stop` | Immediate cancellation (cascades to descendants) |

The runtime still accepts `subagent_close` as a deprecated alias for `subagent_finalize` so older recordings replay cleanly, but it is no longer advertised to the model.

Full semantics and examples: [Subagents (Runtime-Managed)]({{ '/tools/subagents/' | relative_url }}).

## Parent wake-ups

When a subagent completes a turn, the runtime publishes a **compact envelope** to the parent's session as an implicit user-role reminder:

```text
<subagent_update>
subagent_id: ...
agent: researcher
kind: turn_completed
status: waiting
preview: Found three highly-cited papers; one critical piece is by…
Use subagent_inspect to inspect the session or subagent_send to reply.
</subagent_update>
```

The parent only ever consumes envelopes at safe points in its loop, so streamed chat state is never corrupted. If the parent has already finished its own turn but still has in-flight subagents, it waits on the inbox until one of them reports back.

## Configuration

Schema v9 introduces `subagents` as the canonical YAML field. The legacy `sub_agents` spelling is still accepted; see [Agent Configuration]({{ '/configuration/agents/' | relative_url }}#subagents-canonical).

```yaml
version: "9"

agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Research coordinator
    instruction: |
      Plan the task, delegate work to specialists via subagent_start,
      follow up with subagent_send when you need more detail, and
      call subagent_finalize when a subagent is done.
    subagents:
      - researcher
      - writer

  researcher:
    model: openai/gpt-4o
    description: Finds facts, papers, and citations.
    instruction: Be concise. Cite sources.

  writer:
    model: anthropic/claude-sonnet-4-5
    description: Turns research into polished prose.
    instruction: Write clearly and consistently.
```

The agent's `subagents` list controls **which agents** the runtime will allow this parent to start — just like the legacy `sub_agents` list did for `transfer_task`. External references (OCI, URL, `name:reference`) work the same way, see [External subagents from registries](#external-subagents-from-registries).

## Live observability and remote control

The HTTP API exposes every session in the live tree — root or descendant — as a first-class, addressable node:

- `GET /api/sessions/:id/tree` — the full live tree
- `GET /api/live-sessions/:id/attach` — SSE stream for any node
- `POST /api/live-sessions/:id/{steer,followup,close,stop}` — control any node

This is what lets external UIs (dashboards, IDEs, supervisors) watch any subagent's conversation in real time and inject instructions mid-flight. See [Live Sessions]({{ '/features/live-sessions/' | relative_url }}).

## External subagents from registries

Subagents don't have to be defined locally — you can reference agents from OCI registries (such as the [Docker Agent Catalog](https://hub.docker.com/u/agentcatalog)) or URLs directly in your `subagents` list.

```yaml
agents:
  root:
    model: openai/gpt-4o
    description: Coordinator that delegates to local and catalog subagents
    instruction: Delegate tasks to the most appropriate subagent.
    subagents:
      - local_helper
      - agentcatalog/pirate  # pulled from registry automatically

  local_helper:
    model: openai/gpt-4o
    description: A local helper agent for simple tasks
    instruction: You are a helpful assistant.
```

External subagents are automatically named after their last path segment — `agentcatalog/pirate` becomes `pirate`. Use `name:reference` to pick an explicit name:

```yaml
    subagents:
      - my_pirate:agentcatalog/pirate  # available as "my_pirate"
      - reviewer:docker.io/myorg/review-agent:latest
```

External references work with any OCI-compatible registry, not just the Docker Agent Catalog. See [Agent Distribution]({{ '/concepts/distribution/' | relative_url }}).

## Example: development team

```yaml
version: "9"

agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Technical lead coordinating development
    instruction: |
      You are a technical lead managing a development team.
      Analyze requests and delegate to the right specialist.
      Ensure quality by reviewing results before responding.
    subagents: [developer, reviewer, tester]
    toolsets:
      - type: think

  developer:
    model: anthropic/claude-sonnet-4-5
    description: Expert software developer
    instruction: Write clean, efficient code and follow best practices.
    toolsets:
      - type: filesystem
      - type: shell
      - type: think

  reviewer:
    model: openai/gpt-4o
    description: Code review specialist
    instruction: Review code for quality, security, and maintainability.
    toolsets:
      - type: filesystem

  tester:
    model: openai/gpt-4o
    description: Quality assurance engineer
    instruction: Write tests, run the suite, and report results.
    toolsets:
      - type: shell
      - type: todo
```

## Multi-model teams

A key advantage of multi-agent systems is using different models for different roles — picking the best model for each job:

```yaml
models:
  fast:
    provider: openai
    model: gpt-5-mini
    temperature: 0.2 # precise

  creative:
    provider: openai
    model: gpt-4o
    temperature: 0.8 # creative

  local:
    provider: dmr
    model: ai/qwen3 # runs locally, no API cost

agents:
  analyst:
    model: fast     # cheap and fast for analysis
  writer:
    model: creative # creative for content
  helper:
    model: local    # free for simple tasks
```

## Shared tools

Tools like `todo` can be shared between agents for collaborative task tracking:

```yaml
toolsets:
  - type: todo
    shared: true # all agents see the same todo list
```

## Best Practices

- **Keep agents focused.** Each agent should have a clear, narrow role.
- **Write clear descriptions.** The coordinator uses descriptions to decide who to delegate to.
- **Give minimal tools.** Only give each agent the tools it needs for its specific role.
- **Use the right model for each role.** Use capable models for complex reasoning, cheap models for simple tasks.
- **Stick to one multi-agent model per agent.** Do not combine `subagents` with legacy `handoffs` or `background_agents` — pick one.
- **Let subagents own their context.** The parent should delegate specific tasks, not relay every detail — subagents keep their own session and transcript.

## Current rollout status

Runtime-managed subagents are the documented multi-agent model:

- `subagents` is the canonical schema-v9 field name and the sole opt-in for the runtime-managed subagent tools
- live trees can be observed and controlled through the HTTP API

Older multi-agent code paths remain in the product for backward compatibility, but are not covered as first-class workflows in the main docs.

## Example: development team
