# Runtime/API surface for full subagent tree observability and control

This plan focuses on runtime and API/server surfaces only.
TUI work is intentionally deferred.

## Goals

- allow clients to observe the **full agent tree** rooted at a live session
- allow clients to **attach to any live session in the tree** (root, child, grandchild, ...)
- when attached to a child session, stream the **full bidirectional flow** in that session:
  - child assistant output
  - tool calls / tool responses
  - parent->child messages injected into that session
  - descendant-triggered updates that cause the child to react again
- expose enough runtime metadata to render and control the live tree
- add new YAML schema support via **`subagents`** while keeping legacy **`sub_agents`** temporarily

## Design principles

- session is the unit of observability
- every runtime event must be attributable to a specific session id
- tree APIs must work for both:
  - persisted sessions (history)
  - live runtime-only subagent state (current topology / live attachments)
- attaching to a child session must not disturb the running loop
- multiple observers must be allowed on the same live session
- avoid polling where practical; prefer runtime fan-out / subscription

## Deliverables checklist

### 1. Runtime-side live session observability model
- [ ] introduce a runtime-agnostic session tree / live session observer surface
- [ ] add a per-session event fan-out inside local runtime/server path so multiple observers can subscribe to the same live session events
- [ ] ensure root session events and child session events can both be observed independently
- [ ] ensure parent->child injected messages show up as normal session-scoped events in the child session stream
- [ ] ensure child sessions keep emitting across multiple turns to attached observers
- [ ] expose current live tree metadata:
  - [ ] session id
  - [ ] parent session id
  - [ ] agent name
  - [ ] title
  - [ ] status
  - [ ] depth
  - [ ] created at / last update at
  - [ ] root session id

### 2. SessionManager / server live runtime surface
- [ ] add APIs on SessionManager for:
  - [ ] listing live descendants of a root session
  - [ ] getting a live session node by id
  - [ ] attaching to a live session event stream by id
  - [ ] steering/followup to an arbitrary live session in the tree
  - [ ] stopping/closing arbitrary live subagent session in the tree
- [ ] make active runtime bookkeeping tree-aware instead of root-session-only where needed
- [ ] ensure attachments to child sessions continue receiving future turns after the parent re-engages them
- [ ] add tests for multiple observers on the same child session

### 3. HTTP API surface
- [ ] add endpoints for tree metadata, likely along the lines of:
  - [ ] `GET /api/sessions/:id/tree`
  - [ ] `GET /api/live-sessions/:id`
  - [ ] `GET /api/live-sessions/:id/attach` (SSE)
  - [ ] `POST /api/live-sessions/:id/steer`
  - [ ] `POST /api/live-sessions/:id/followup`
  - [ ] `POST /api/live-sessions/:id/close`
  - [ ] `POST /api/live-sessions/:id/stop`
- [ ] return clear 404/409 semantics for unknown vs non-live sessions
- [ ] extend the HTTP client / remote client interfaces accordingly
- [ ] register all new event types in the client decoder
- [ ] add server tests for attach / tree / child steering

### 4. Runtime event model hardening
- [ ] verify all events needed for observation are truly session-scoped
- [ ] add any missing session-scoped events for child-session lifecycle / parent-message injection
- [ ] ensure live observers see enough information to reconstruct bidirectional transcript
- [ ] ensure persistence filtering still remains correct for parent vs child sessions

### 5. YAML schema + config versioning
- [ ] add config v9 package
- [ ] make latest version = 9
- [ ] add new field `subagents` to latest/v9 agent schema
- [ ] keep legacy `sub_agents` accepted temporarily in v9/latest parser
- [ ] define precedence / conflict behavior when both are present
- [ ] update migration from v8 -> v9
- [ ] update validation to use the normalized field
- [ ] update `agent-schema.json`
- [ ] add config tests for:
  - [ ] `subagents` only
  - [ ] legacy `sub_agents` only
  - [ ] both present
  - [ ] external refs still work

### 6. Documentation
- [ ] document live session tree / attach model
- [ ] document attach semantics for child sessions
- [ ] document new API endpoints
- [ ] document schema v9 and `subagents`

## Validation targets
- [ ] focused runtime tests
- [ ] server/API tests
- [ ] config/schema tests
- [ ] `go test ./...`
- [ ] lint clean on touched packages
