---
title: "Live Sessions (Tree + Attach)"
description: "Observe and control any node of a live runtime-managed subagent tree over HTTP, including attaching to a child session's event stream without disturbing its loop."
permalink: /features/live-sessions/
---

# Live Sessions

_Observe and control any node of a live runtime-managed subagent tree over HTTP, including attaching to a child session's event stream without disturbing its loop._

## Overview

When the [API Server]({{ '/features/api-server/' | relative_url }}) (`docker agent serve api`) is running, every session owned by the runtime — the root session **and** every [runtime-managed subagent]({{ '/tools/subagents/' | relative_url }}) descended from it — is a first-class, addressable node. Clients can:

- Get the full live **tree** rooted at a session
- Look up a specific **live node** by session id
- **Attach** to a live session's event stream (root or descendant) over Server-Sent Events
- **Steer**, **follow-up**, **close**, or **stop** arbitrary nodes

This makes it possible to build dashboards, IDE integrations, and supervisory UIs that don't just watch the root agent — they can watch and intervene at any point in the live agent tree.

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ Scope
</div>
  <p>These endpoints only expose sessions that are currently live in the runtime. For historical/persisted sessions use the existing <code>GET /api/sessions/:id</code> endpoint (see <a href="{{ '/features/api-server/' | relative_url }}">API Server</a>). An attached client that connects mid-session only receives events from the attach point forward — combine with <code>GET /api/sessions/:id</code> for the full transcript.</p>

</div>

## Endpoints

All endpoints live under the `/api` prefix on the API server.

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/sessions/:id/tree` | Full live tree rooted at `:id` |
| `GET` | `/api/live-sessions/:id` | Metadata for a single live session (root or descendant) |
| `GET` | `/api/live-sessions/:id/attach` | SSE stream of live events for `:id` |
| `POST` | `/api/live-sessions/:id/steer` | Inject user messages mid-turn into `:id` |
| `POST` | `/api/live-sessions/:id/followup` | Queue messages for end-of-turn processing on `:id` |
| `POST` | `/api/live-sessions/:id/close` | Ask `:id` to finish cleanly after its next safe point |
| `POST` | `/api/live-sessions/:id/stop` | Forcibly cancel `:id` |

Error semantics:

- `404 Not Found` — `:id` is not a known live root or descendant
- `409 Conflict` — the target session cannot accept the control action right now (e.g. already terminal)

## Live session node

Every endpoint returns or references a `LiveSessionNode`:

```json
{
  "session_id": "01HXYZ…",
  "parent_session_id": "01HABC…",
  "root_session_id": "01HABC…",
  "agent_name": "researcher",
  "title": "Research: agent protocols",
  "kind": "subagent",
  "depth": 1,
  "status": "waiting",
  "created_at": "2026-04-20T12:34:56Z",
  "last_update_at": "2026-04-20T12:35:12Z",
  "last_preview": "Found three highly-cited papers; one critical piece is by…",
  "error": ""
}
```

| Field | Meaning |
| --- | --- |
| `kind` | `"root"` for the root session, `"subagent"` for a runtime-managed subagent |
| `depth` | `0` for the root, `1` for direct children, etc. |
| `status` | `starting` · `running` · `waiting` · `closed` · `stopped` · `failed` |
| `last_preview` | Short excerpt of the subagent's last assistant message (truncated) |

## Get the live tree

```bash
$ curl http://localhost:8080/api/sessions/$ROOT_ID/tree
{
  "root_session_id": "01HABC…",
  "nodes": [
    { "session_id": "01HABC…", "kind": "root",     "depth": 0, "status": "running", ... },
    { "session_id": "01HXYZ…", "kind": "subagent", "depth": 1, "status": "waiting", "agent_name": "researcher", ... },
    { "session_id": "01HQRS…", "kind": "subagent", "depth": 1, "status": "running", "agent_name": "writer",     ... },
    { "session_id": "01HTUV…", "kind": "subagent", "depth": 2, "parent_session_id": "01HQRS…", ... }
  ]
}
```

Nodes are ordered by creation time. The root is always included when the runtime owns that root session.

## Attach to a live session

The `attach` endpoint opens an SSE connection and streams every event the runtime emits on the target session from the moment of attach until the client disconnects or the session becomes terminal.

```bash
$ curl -N http://localhost:8080/api/live-sessions/$CHILD_ID/attach

data: {"type":"stream_started","session_id":"01HXYZ…","agent":"researcher"}
data: {"type":"agent_choice","session_id":"01HXYZ…","content":"Let me search for"}
data: {"type":"tool_call","session_id":"01HXYZ…","function":{"name":"search"}}
data: {"type":"tool_call_response","session_id":"01HXYZ…",...}
data: {"type":"agent_choice","session_id":"01HXYZ…","content":"Found three papers:..."}
data: {"type":"stream_stopped","session_id":"01HXYZ…"}
# … later, parent sends a follow-up message to this child:
data: {"type":"user_message","session_id":"01HXYZ…","content":"Also include one skeptical piece."}
data: {"type":"stream_started","session_id":"01HXYZ…","agent":"researcher"}
```

Multiple clients may attach to the same live session simultaneously. Each attachment receives its own stream; the runtime fans events out via a per-session event bus.

Notable behavior:

- **Parent→child messages are visible** on the child's stream as a normal `user_message` event, even though they were injected via `/api/live-sessions/:id/steer` rather than typed by a user.
- **Descendant-triggered wake-ups** cause the attached child session to begin a new turn automatically; you will see `stream_started` and subsequent events on the child's stream without the parent doing anything.
- An attached session that becomes terminal (`closed` / `stopped` / `failed`) will close its stream after flushing its final events.

## Steer / follow-up

`steer` injects messages at the next mid-turn safe point. `followup` queues them to run after the current turn ends. Both take the same payload shape as the existing `/api/sessions/:id/steer` endpoint:

```bash
$ curl -X POST http://localhost:8080/api/live-sessions/$CHILD_ID/steer \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Also include one skeptical or critical paper."}]}'
{"status":"queued"}
```

For a subagent, both endpoints ultimately deliver a parent→child message through the subagent manager's inbox — the distinction between "mid-turn" and "end-of-turn" only affects the root session's own loop.

## Close / stop

```bash
# Graceful
$ curl -X POST http://localhost:8080/api/live-sessions/$CHILD_ID/close
{"status":"closing"}

# Forceful
$ curl -X POST http://localhost:8080/api/live-sessions/$CHILD_ID/stop
{"status":"stopping"}
```

Both operations **cascade**: every live descendant of the target is also asked to stop. The runtime then emits a terminal envelope (`subagent_update` with kind `closed` / `stopped`) on the parent session, and a matching `stream_stopped` on the target child's stream before its SSE ends.

Close/stop on a root session is not supported through these endpoints; delete the session via `DELETE /api/sessions/:id` instead.

## Go client

The `runtime.Client` type in `pkg/runtime` exposes typed helpers for every endpoint:

```go
tree, err := client.GetLiveSessionTree(ctx, rootSessionID)
node, err := client.GetLiveSession(ctx, childID)
events, err := client.AttachLiveSession(ctx, childID)
err = client.SteerLiveSession(ctx, childID, []api.Message{{Content: "more details please"}})
err = client.FollowUpLiveSession(ctx, childID, []api.Message{{Content: "wrap up now"}})
err = client.CloseLiveSession(ctx, childID)
err = client.StopLiveSession(ctx, childID)
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">💡 See also
</div>
  <p><a href="{{ '/tools/subagents/' | relative_url }}">Subagents (Runtime-Managed)</a> for the tools the parent uses to drive its subagents, <a href="{{ '/features/api-server/' | relative_url }}">API Server</a> for the root-session endpoints, and <a href="https://github.com/docker/docker-agent/blob/main/docs/design/subagent-runtime-api-plan.md">the design doc</a> for the underlying runtime model.</p>
</div>
