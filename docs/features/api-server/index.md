---
title: "API Server"
description: "Expose your agents via an HTTP API for programmatic access, web frontends, and integrations."
permalink: /features/api-server/
---

# API Server

_Expose your agents via an HTTP API for programmatic access, web frontends, and integrations._

## Overview

The `docker agent serve api` command starts an HTTP server that exposes your agents through a REST-style API with Server-Sent Events (SSE) streaming. Use it to build web UIs, integrate with CI/CD pipelines, or connect agents to other services.

When you use the runtime-managed subagent subsystem, the API server also exposes the full **live session tree** rooted at any running session: you can inspect descendants, attach to a child session's event stream, and steer or stop arbitrary nodes. See [Live Sessions]({{ '/features/live-sessions/' | relative_url }}).

```bash
# Start the API server
$ docker agent serve api agent.yaml

# Custom listen address
$ docker agent serve api agent.yaml --listen 0.0.0.0:8080

# With session persistence
$ docker agent serve api agent.yaml --session-db ./sessions.db

# Auto-refresh from OCI registry every 10 minutes
$ docker agent serve api agentcatalog/coder --pull-interval 10
```

## Endpoints

All endpoints are under the `/api` prefix.

### Agents

| Method | Path              | Description                       |
| ------ | ----------------- | --------------------------------- |
| `GET`  | `/api/agents`     | List all available agents         |
| `GET`  | `/api/agents/:id` | Get an agent's full configuration |

### Sessions

| Method   | Path                                | Description                                         |
| -------- | ----------------------------------- | --------------------------------------------------- |
| `GET`    | `/api/sessions`                     | List all sessions                                   |
| `POST`   | `/api/sessions`                     | Create a new session                                |
| `GET`    | `/api/sessions/:id`                 | Get a session by ID (messages, tokens, permissions) |
| `DELETE` | `/api/sessions/:id`                 | Delete a session                                    |
| `PATCH`  | `/api/sessions/:id/title`           | Update session title                                |
| `PATCH`  | `/api/sessions/:id/permissions`     | Update session permissions                          |
| `POST`   | `/api/sessions/:id/resume`          | Resume a paused session (after tool confirmation)   |
| `POST`   | `/api/sessions/:id/tools/toggle`    | Toggle auto-approve (YOLO) mode                     |
| `POST`   | `/api/sessions/:id/elicitation`     | Respond to an MCP tool elicitation request          |

### Agent Execution

| Method | Path                                   | Description                                   |
| ------ | -------------------------------------- | --------------------------------------------- |
| `POST` | `/api/sessions/:id/agent/:agent`       | Run the root agent for a session (SSE stream) |
| `POST` | `/api/sessions/:id/agent/:agent/:name` | Run a specific named agent (SSE stream)       |

**Path parameters:**

- **`:agent`** — The agent identifier, which is the **config filename without the `.yaml` extension**. This must match the filename passed to `docker agent serve api`. For example, if you start the server with `docker agent serve api my-assistant.yaml`, the agent identifier is `my-assistant`. When serving a directory of YAML files, each file becomes a separate agent identified by its filename without the extension.
- **`:name`** _(optional)_ — The name of a specific sub-agent defined in a multi-agent configuration. If omitted, the request targets the `root` agent. For example, in a config that defines agents named `root`, `coder`, and `reviewer`, use `/api/sessions/:id/agent/my-config/coder` to run the `coder` sub-agent directly.

**Examples:**

```bash
# Single-agent config: my-assistant.yaml
# Start: docker agent serve api my-assistant.yaml
# Run the root agent:
curl -N -X POST http://localhost:8080/api/sessions/$SID/agent/my-assistant \
  -H "Content-Type: application/json" \
  -d '[{"role": "user", "content": "Hello!"}]'

# Multi-agent config: team.yaml (defines agents: root, coder, reviewer)
# Start: docker agent serve api team.yaml
# Run the root agent:
curl -N -X POST http://localhost:8080/api/sessions/$SID/agent/team \
  -H "Content-Type: application/json" \
  -d '[{"role": "user", "content": "Review this PR"}]'

# Run a specific sub-agent (reviewer):
curl -N -X POST http://localhost:8080/api/sessions/$SID/agent/team/reviewer \
  -H "Content-Type: application/json" \
  -d '[{"role": "user", "content": "Review this PR"}]'
```

### Live sessions / subagent tree

| Method | Path | Description |
| ------ | ---- | ----------- |
| `GET`  | `/api/sessions/:id/tree` | Get the currently-live agent/subagent tree rooted at `:id` |
| `GET`  | `/api/live-sessions/:id` | Get metadata for a live session node (root or descendant) |
| `GET`  | `/api/live-sessions/:id/attach` | Attach to a live session's SSE stream |
| `POST` | `/api/live-sessions/:id/steer` | Inject user messages into a live root or descendant session |
| `POST` | `/api/live-sessions/:id/followup` | Queue end-of-turn messages for a live root or descendant session |
| `POST` | `/api/live-sessions/:id/close` | Ask a live descendant session to close cleanly |
| `POST` | `/api/live-sessions/:id/stop` | Forcibly stop a live descendant session |

See [Live Sessions]({{ '/features/live-sessions/' | relative_url }}) for the detailed event model and examples.

### Health

| Method | Path        | Description                               |
| ------ | ----------- | ----------------------------------------- |
| `GET`  | `/api/ping` | Health check — returns `{"status": "ok"}` |

## Streaming Responses

The agent execution endpoints (`POST /api/sessions/:id/agent/:agent`) return **Server-Sent Events (SSE)**. Each event is a JSON object representing a runtime event (remember that `:agent` is the config filename without the `.yaml` extension):

```bash
# Send a message and stream the response
# (assuming the server was started with: docker agent serve api my-agent.yaml)
$ curl -N -X POST http://localhost:8080/api/sessions/$SID/agent/my-agent \
  -H "Content-Type: application/json" \
  -d '[{"role": "user", "content": "Hello!"}]'

# Response (SSE stream):
data: {"type":"stream_started","session_id":"...","agent":"root"}
data: {"type":"agent_choice","content":"Hello! How","agent":"root"}
data: {"type":"agent_choice","content":" can I help","agent":"root"}
data: {"type":"agent_choice","content":" you today?","agent":"root"}
data: {"type":"stream_stopped","session_id":"...","agent":"root"}
```

Event types include:

- `stream_started` / `stream_stopped` — Agent execution lifecycle
- `agent_choice` — Streamed text content (partial responses)
- `tool_call` — Agent requesting tool execution
- `tool_call_confirmation` — Tool call waiting for user approval
- `tool_call_response` — Tool execution result
- `subagent_started` — A runtime-managed subagent session was created
- `subagent_sent` — The parent sent a follow-up message to a live subagent
- `subagent_update` — A child envelope was injected into the parent session
- `error` — Error during execution

If you attach to a child session with `/api/live-sessions/:id/attach`, you also see parent→child injected messages as normal `user_message` events on that child session stream.

## Typical Workflow

1. **List agents** — `GET /api/agents` to discover available agents
2. **Create session** — `POST /api/sessions` to start a conversation
3. **Send message** — `POST /api/sessions/:id/agent/:agent` with user messages
4. **Stream response** — Read SSE events as the agent processes
5. **Handle confirmations** — If a tool call needs approval, `POST /api/sessions/:id/resume`
6. **Continue** — Send follow-up messages to the same session

```bash
# 1. List available agents
$ curl http://localhost:8080/api/agents
[{"name":"my-agent","multi":false,"description":"A helpful assistant"}]

# 2. Create a session
$ curl -X POST http://localhost:8080/api/sessions \
  -H "Content-Type: application/json" -d '{}'
{"id":"abc-123","title":"","created_at":"..."}

# 3. Run the agent with a message
$ curl -N -X POST http://localhost:8080/api/sessions/abc-123/agent/my-agent \
  -H "Content-Type: application/json" \
  -d '[{"role":"user","content":"What files are in the current directory?"}]'
```

## CLI Flags

```bash
docker agent serve api <agent-file>|<agents-dir> [flags]
```

| Flag               | Default          | Description                                      |
| ------------------ | ---------------- | ------------------------------------------------ |
| `-l, --listen`     | `127.0.0.1:8080` | Address to listen on                             |
| `-s, --session-db` | `session.db`     | Path to the SQLite session database              |
| `--pull-interval`  | `0` (disabled)   | Auto-pull OCI reference every N minutes          |
| `--fake`           | (none)           | Replay AI responses from cassette file (testing) |
| `--record`         | (none)           | Record AI API interactions to cassette file      |

<div class="callout callout-tip" markdown="1">
<div class="callout-title">💡 Multi-agent configs
</div>
  <p>You can point <code>docker agent serve api</code> at a directory containing multiple agent YAML files. Each becomes a separate agent accessible via <code>/api/agents</code>. Combine with <code>--pull-interval</code> to auto-refresh agents from an OCI registry.</p>

</div>

## Session Persistence

Sessions are stored in a SQLite database (default: `session.db` in the current directory). This means:

- Sessions survive server restarts
- Multiple server instances can share a database
- Use `--session-db` to specify a custom path

## Tool Call Approval

By default, tool calls require approval. In the API workflow:

1. Agent makes a tool call → server emits a `tool_call_confirmation` event
2. Client reviews and sends `POST /api/sessions/:id/resume` with the decision
3. Execution continues based on approval/denial

Toggle auto-approve with `POST /api/sessions/:id/tools/toggle` for automated workflows.

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ See also
</div>
  <p>For interactive use, see the <a href="{{ '/features/tui/' | relative_url }}">Terminal UI</a>. For runtime-managed multi-agent observability and control, see <a href="{{ '/features/live-sessions/' | relative_url }}">Live Sessions</a> and <a href="{{ '/tools/subagents/' | relative_url }}">Subagents</a>. For agent-to-agent communication, see <a href="{{ '/features/a2a/' | relative_url }}">A2A Protocol</a> and <a href="{{ '/features/acp/' | relative_url }}">ACP</a>. For MCP integration, see <a href="{{ '/features/mcp-mode/' | relative_url }}">MCP Mode</a>.</p>

</div>
