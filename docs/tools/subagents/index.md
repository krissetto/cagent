---
title: "Subagents (Runtime-Managed)"
description: "Persistent, background, conversational subagents with two-way messaging, nested delegation, and a clean replacement for legacy transfer_task / handoff / background_agents."
permalink: /tools/subagents/
---

# Subagents (Runtime-Managed)

_Persistent, background, conversational subagents with two-way messaging, nested delegation, and a clean replacement for legacy `transfer_task` / `handoff` / `background_agents`._

## Overview

Runtime-managed subagents are docker-agent's recommended way to build multi-agent systems. A parent agent can:

- **Start** a subagent that runs on its own session, in the background
- **Send** follow-up messages to it at any time (true two-way conversation)
- **Inspect** its transcript and status on demand
- **Close** it gracefully, or **stop** it immediately
- **Receive wake-ups** whenever the subagent completes a turn — without polling

Every subagent lives on its **own session**, which means it has its own message history, token accounting, events, and observability surface. Subagents can themselves start further subagents, up to a configurable depth cap.

<div class="callout callout-warning" markdown="1">
<div class="callout-title">⚠️ Do not mix with legacy multi-agent features
</div>
  <p>This subsystem <strong>replaces</strong> <code>transfer_task</code>, <code>handoff</code>, and <code>background_agents</code>. Do <strong>not</strong> combine them in the same agent — the legacy tools and this subsystem are not designed to coexist, and mixing them will produce confusing behavior, duplicated delegation surfaces, and inconsistent session trees.</p>
  <p>See <a href="#migrating-from-legacy-multi-agent">Migrating from legacy multi-agent</a>.</p>

</div>

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ Rollout status
</div>
  <p>The runtime, the six canonical subagent tools, the deprecated-but-still-accepted <code>subagent_close</code> alias, and the live-session API are now shipped. Schema v9 also makes <code>subagents</code> the canonical config field, while the legacy <code>sub_agents</code> spelling remains accepted for backward compatibility.</p>
  <p>Top-level <code>subagents:</code> is now the sole opt-in for the runtime-managed subsystem: it both declares which child agents may be started and enables the six <code>subagent_*</code> tools. Do <strong>not</strong> also configure legacy <code>handoffs:</code> or <code>- type: background_agents</code> on that same agent — config validation rejects those combinations.</p>

</div>

## Why a new subsystem?

The legacy tools each solve a slice of the problem but cannot be composed cleanly:

| Need | Legacy | Runtime-managed subagents |
| --- | --- | --- |
| Parent blocks waiting for a child | `transfer_task` | ✅ (by design when the parent keeps turning) |
| Parent can keep working while child runs | `background_agents` | ✅ |
| Parent can send more messages to a running child | — | ✅ (`subagent_send`) |
| Child wakes parent when it has news | — | ✅ (compact envelope, no polling) |
| Nested delegation (child spawns its own child) | mostly no | ✅, with depth cap |
| Clean per-session observability + control via the API | root-only | ✅ at every node |
| Peer-to-peer "hand off the whole conversation" routing | `handoff` | not this tool — see [Handoffs](#what-about-handoffs) |

The runtime-managed subsystem is a single cohesive model: every child is a session, every interaction is explicit, and the HTTP API can observe and steer any node in the live tree.

## The Canonical Tools

The runtime registers these tools directly on the agent loop:

| Tool | Purpose |
| --- | --- |
| `subagent_start` | Create a new subagent conversation on its own session |
| `subagent_send` | Send a follow-up message to a live subagent |
| `subagent_list` | List the subagents owned by the current session |
| `subagent_inspect` | Return status + the latest assistant response by default (optionally a recent slice or full transcript) |
| `subagent_finalize` | Ask a subagent to finish cleanly after its current safe point |
| `subagent_stop` | Cancel a subagent immediately |

The runtime also still accepts `subagent_close` as a deprecated alias for `subagent_finalize` so older session recordings keep replaying, but the alias is no longer advertised to the model.

All six canonical tools are auto-approved: the runtime considers them a trusted internal coordination surface, analogous to how the legacy `transfer_task` tool is always auto-approved.

### `subagent_start`

```json
{
  "agent": "researcher",
  "task": "Find the three most-cited papers on agent-to-agent protocols published since 2023."
}
```

Returns a short `subagent_id` the parent uses for every subsequent interaction. The id exposed to the model is a **5-character short ref** derived from the underlying session id; it is stable for the lifetime of that subagent and is the form returned by `subagent_start`, `subagent_list`, `subagent_inspect`, and the envelope updates the runtime injects on wake-up. Full session ids are still accepted as input for compatibility. The target agent must be reachable from the parent via the parent agent's `subagents` list (see [Configuration](#configuration)).

`subagent_start` keeps the delegation surface intentionally small: the parent gives the child an `agent` and a `task`, and anything more specific about output shape should be folded directly into the task text.

### `subagent_send`

Send a follow-up message (default):

```json
{
  "subagent_id": "...",
  "message": "Also include one skeptical or critical paper."
}
```

Send a steer-mode message for mid-turn injection:

```json
{
  "subagent_id": "...",
  "message": "Change direction: focus on regulatory papers instead.",
  "mode": "steer"
}
```

The optional `mode` parameter controls delivery timing:

| Mode | Delivery |
| --- | --- |
| `"followup"` (default) | Between turns — the child finishes its current turn, then processes the message as a new user turn. |
| `"steer"` | Mid-turn — delivered at the next safe point during a running child turn (after a tool batch, before the next model call). If the child is idle, it behaves like a follow-up. |

Use `"steer"` when the parent needs to course-correct a long-running child without waiting for it to finish. Use the default when a normal conversational follow-up is sufficient.

### `subagent_list`

No parameters. Returns a compact JSON list of the calling session's subagents, each with its ID, agent name, status, creation time, and last preview.

### `subagent_inspect`

```json
{ "subagent_id": "..." }
```

By default, returns the subagent's current status and its **last assistant message in full**. This is the recommended, cost-aware path when the parent just wants the latest answer without pulling extra transcript context into its own window.

Need more context? Opt in explicitly:

```json
{ "subagent_id": "...", "mode": "recent" }
```

- `mode: "last"` (default) — status + last assistant message only
- `mode: "recent"` — also include up to the last 6 non-system messages
- `mode: "full"` — include the full non-system transcript, truncated if needed to keep the tool response bounded (~64KB)

Use `recent` when the last reply references earlier turns you haven't seen; use `full` sparingly because it can still consume a lot of context and tokens even with the runtime cap.

### `subagent_finalize`

```json
{ "subagent_id": "..." }
```

Signals the child to terminate cleanly after it finishes its current safe point. Finalize **cascades**: every live descendant of the target is also asked to stop.

`subagent_close` is still accepted as a deprecated alias so existing sessions keep deserializing, but it is intentionally no longer advertised to new prompts. New callers should use `subagent_finalize`.

### `subagent_stop`

```json
{ "subagent_id": "..." }
```

Forcibly cancels the child immediately. Stop also cascades to descendants.

## How wake-ups work

When a subagent finishes a turn, the runtime publishes a **compact envelope** to its parent session's inbox. The envelope is injected into the parent's conversation as an implicit user-role reminder:

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

Rules:

- The parent only sees envelopes at **safe points** in its loop — after a tool batch, or after it finishes its own turn.
- Envelopes contain a truncated preview (default ~800 characters). The full message is always available via `subagent_inspect`.
- `subagent_inspect` is cheap by default: unless you explicitly ask for `mode: "recent"` or `mode: "full"`, it returns only the latest assistant message and status metadata, not the prior back-and-forth transcript.
- A waiting subagent does not block its parent forever — once the parent's own turn ends, the runtime waits on the child's inbox only if the child is still in-flight or has queued work.
- When a grandchild publishes an envelope, the middle child wakes automatically and re-engages without polling.

Deeper rationale and diagrams: [`docs/design/subagent-runtime.md`](https://github.com/docker/docker-agent/blob/main/docs/design/subagent-runtime.md).

## Recursive delegation

Subagents can spawn subagents. The runtime enforces two safety caps:

| Cap | Default | Meaning |
| --- | --- | --- |
| Maximum depth | `8` | Depth 1 = direct children of the root, depth 2 = grandchildren, etc. |
| Maximum descendants per tree | `64` | Total number of live subagents anywhere beneath a single root session |

These caps are applied before the child loop is started, so a rejected `subagent_start` never leaves a partially-started session behind. Exceeding them raises an error the parent tool handler surfaces back to the model.

Closing or stopping a subagent **cascades** to every descendant, and context cancellation propagates down the tree too.

## Events

The runtime emits three session-scoped events for this subsystem:

| Event | Emitted on |
| --- | --- |
| `subagent_started` | `subagent_start` succeeded |
| `subagent_sent` | `subagent_send` delivered a message to the child |
| `subagent_update` | A child envelope was injected into the parent's session |

Combined with the existing per-session events (assistant messages, tool calls, token usage, etc.), this is enough for an external client to reconstruct the full bidirectional transcript of every session in the live tree. See [Live Sessions]({{ '/features/live-sessions/' | relative_url }}) for the HTTP surface.

## Configuration

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ Canonical field name
</div>
  <p>Schema v9 introduces <code>subagents</code> as the canonical YAML field. The legacy <code>sub_agents</code> spelling is still accepted for backward compatibility and automatically normalized during config load. New configs should use <code>subagents</code>. Mixing both in the same agent is allowed — the canonical <code>subagents</code> wins — but discouraged. See <a href="{{ '/configuration/agents/' | relative_url }}#subagents-canonical">Agent Config</a>.</p>

</div>

A parent agent can only start subagents listed in its `subagents` array:

```yaml
version: "9"

agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Coordinator
    instruction: |
      Plan the work and delegate specialist tasks to your subagents.
      Use subagent_start to dispatch, subagent_send to follow up,
      subagent_inspect when a preview isn't enough, and
      subagent_finalize / subagent_stop when you're done with a subagent.
    subagents:
      - researcher
      - writer

  researcher:
    model: openai/gpt-4o
    description: Finds facts, papers, and citations.
    instruction: Be concise. Cite sources.

  writer:
    model: anthropic/claude-sonnet-4-5
    description: Turns research notes into a polished draft.
    instruction: Write with a clear structure and a consistent voice.
```

External references (OCI / URLs) work exactly the same way they do for the legacy `sub_agents` field, including the `name:reference` naming shorthand:

```yaml
    subagents:
      - reviewer:agentcatalog/review-pr
      - agentcatalog/pirate
```

See [External Sub-Agents]({{ '/concepts/multi-agent/#external-subagents-from-registries' | relative_url }}).

## Live observability + remote control

The HTTP API server exposes the live tree so external UIs can attach to any node — not just the root session — stream its events, and inject messages / close / stop arbitrary nodes. See [Live Sessions]({{ '/features/live-sessions/' | relative_url }}) for the full endpoint reference.

## Migrating from legacy multi-agent

You're migrating if your config currently uses any of these together:

- `sub_agents:` / `subagents:` (auto-enables `transfer_task`)
- `handoffs:`
- `- type: background_agents`

### Before (legacy)

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    sub_agents: [researcher, writer]
    toolsets:
      - type: background_agents
    handoffs:
      - summarizer
```

### After (runtime-managed subagents)

```yaml
version: "9"

agents:
  root:
    model: anthropic/claude-sonnet-4-5
    subagents: [researcher, writer]
    # No background_agents toolset.
    # No handoffs — see note below.
```

Key rules when migrating:

1. **Rename** `sub_agents:` → `subagents:`. Both spellings are accepted but `subagents` is canonical.
2. **Remove** `- type: background_agents` from your `toolsets:`. The new subsystem covers the parallel-dispatch use case natively via `subagent_start` + `subagent_send`.
3. **Rethink `handoffs:`**. Handoffs are a different shape (peer-to-peer single-session routing, not hierarchical delegation) — see [What about handoffs?](#what-about-handoffs).
4. **Do not mix.** If your agent has `subagents:` you should not also enable `background_agents` or `handoffs` on the same agent. Config validation rejects those combinations.

### What about handoffs?

`handoff` solves a different shape: the entire conversation moves to another agent and stays there. Runtime-managed subagents are hierarchical — the parent always retains control. If you have a real handoff workflow today (e.g. a coordinator routes to a specialist, the specialist routes back), that pattern will eventually be expressible using the runtime-managed subagent tools + explicit control flow, but a direct 1:1 replacement for `handoff` is not yet in place. Track the deprecation plan for handoffs in the design docs:

- [`docs/design/subagent-runtime.md`](https://github.com/docker/docker-agent/blob/main/docs/design/subagent-runtime.md)
- [`docs/design/subagent-runtime-plan.md`](https://github.com/docker/docker-agent/blob/main/docs/design/subagent-runtime-plan.md)

Until then: do **not** put `handoffs:` on an agent that also uses `subagents:`. Pick one model.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">💡 See also
</div>
  <p><a href="{{ '/concepts/multi-agent/' | relative_url }}">Multi-Agent Systems</a> for the overall concept, <a href="{{ '/configuration/agents/' | relative_url }}">Agent Configuration</a> for the canonical <code>subagents</code> field, and <a href="{{ '/features/live-sessions/' | relative_url }}">Live Sessions</a> for the HTTP observability and control surface.</p>
</div>
