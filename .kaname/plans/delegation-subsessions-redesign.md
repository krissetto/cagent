# Delegation as Persistent Agent-to-Agent Subsessions — Redesign Plan

**Branch:** `better-subagents`  
**Date:** 2026-04-04  
**Replaces:** `unified-delegation-redesign.md`

---

## Design Summary

- [ ] Model delegation as persistent **subsessions**.
- [ ] Each delegated child runs in its own normal `session.Session` with its own configured system messages.
- [ ] The parent starts a child by sending a **normal user message**.
- [ ] Later parent replies continue the same child session via `delegation_id`.
- [ ] A child response may be a question, not only a final result.
- [ ] Delegation sessions are tracked as subsessions and hidden from normal user-started session lists by default.
- [ ] Parent transcript must never be polluted with child internal transcript.

---

## Core Invariants

- [ ] Child sessions are ordinary sessions using the child agent's own instruction/tool prompts.
- [ ] No synthetic delegation system prompt.
- [ ] No implicit `Please proceed.` message.
- [ ] The first child message is a normal user message with the delegated task.
- [ ] `delegation_id` is the child session ID.
- [ ] Delegations are continuable across turns.
- [ ] Parent session stores only the tool call + returned child reply, not the child transcript.
- [ ] Child streaming is not forwarded into the parent transcript/event stream.
- [ ] Session parent/child relationships are sufficient to reconstruct the delegation tree.

---

## Phase 1 — Session Model

- [ ] Reuse `session.Session.ParentID` as the persistent subsession link.
- [ ] Verify root session listings exclude sessions where `ParentID != ""`.
- [ ] Add store support to query child sessions by parent ID.
- [ ] Remove legacy assumptions that subsessions are only embedded transcript artifacts.
- [ ] Remove task-wrapper prompt/session hacks from session construction.

### Files
- [ ] `pkg/session/session.go`
- [ ] `pkg/session/store.go`
- [ ] `pkg/session/migrations.go` if needed

---

## Phase 2 — Delegation Domain Rewrite

- [ ] Rewrite delegation state around persistent child sessions.
- [ ] Remove in-memory output buffering as the primary model.
- [ ] Remove synthetic parent/child delegation tree bookkeeping where session links suffice.
- [ ] Keep lifecycle state for running/completed/failed/cancelled.
- [ ] Support sync and async launches.
- [ ] Support continuation of an existing delegation.
- [ ] Support cancellation of a running delegation.

### Files
- [ ] `pkg/runtime/delegation/delegation.go`
- [ ] `pkg/runtime/delegation/manager.go`
- [ ] `pkg/runtime/delegation/manager_test.go`

---

## Phase 3 — Runtime Integration Rewrite

- [ ] Rewrite `handleDelegate` around real subsessions.
- [ ] Add support for continuing an existing delegation via `delegation_id`.
- [ ] Delete legacy background-agent and transfer-task handlers.
- [ ] Keep handoff separate from delegation.
- [ ] Ensure child runs do not forward transcript content into parent transcript.
- [ ] Ensure child completion returns the latest child assistant reply.

### Files
- [ ] `pkg/runtime/agent_delegation.go`
- [ ] `pkg/runtime/runtime.go`
- [ ] `pkg/runtime/loop.go`

---

## Phase 4 — Tool Surface Simplification

- [ ] Simplify the built-in tool surface.
- [ ] Keep a single `delegate` tool for starting or continuing a child conversation.
- [ ] Keep `stop_delegation`.
- [ ] Remove legacy background-agent and transfer-task compatibility surface.
- [ ] Remove handoff mode from `delegate`.

### Desired tool shape
- [ ] `delegate(agent, message)` for a new child session
- [ ] `delegate(delegation_id, message)` or equivalent for continuing a child session
- [ ] `stop_delegation(delegation_id)`

### Files
- [ ] `pkg/tools/builtin/delegate.go`
- [ ] `pkg/tools/builtin/transfertask.go` (remove or stop using)
- [ ] `pkg/tools/builtin/agent/agent.go` (remove background agent toolset path)
- [ ] `pkg/tools/builtin/delegate_test.go`

---

## Phase 5 — Event Model Cleanup

- [ ] Keep only lifecycle events that are actually useful.
- [ ] Remove output-buffer/tree snapshot events tied to the old model.
- [ ] Ensure async completion signaling remains available.
- [ ] Ensure child choice/reasoning events are not surfaced into the parent chat stream.

### Files
- [ ] `pkg/runtime/event.go`
- [ ] `pkg/runtime/runtime.go`

---

## Phase 6 — Persistence

- [ ] Ensure child sessions persist as ordinary sessions with `ParentID` set.
- [ ] Ensure child sessions are not shown in default root-session lists.
- [ ] Ensure delegation tree can be reconstructed from persisted session links.
- [ ] Decide and implement the cleanest persistence path for child runs.

### Files
- [ ] `pkg/runtime/persistent_runtime.go`
- [ ] `pkg/session/store.go`

---

## Phase 7 — Loader / Config Cleanup

- [ ] Stop registering legacy transfer/background-agent tool surfaces.
- [ ] Register only the new delegation tool surface for agents with subagents.
- [ ] Remove background-agent toolset plumbing.
- [ ] Remove unneeded backward-compat config/schema paths.

### Files
- [ ] `pkg/teamloader/teamloader.go`
- [ ] `pkg/teamloader/registry.go`
- [ ] `pkg/config/latest/validate.go`
- [ ] `agent-schema.json`

---

## Phase 8 — Tests

- [ ] Add unit tests for sync delegation lifecycle.
- [ ] Add unit tests for async delegation lifecycle.
- [ ] Add unit tests for continuing an existing delegation.
- [ ] Add unit tests for child asking a question and parent replying.
- [ ] Add unit tests for cancellation.
- [ ] Add unit tests for depth/concurrency limits.
- [ ] Add handler tests proving:
  - [ ] no `Please proceed.`
  - [ ] child uses its own system messages
  - [ ] first child message is the delegated task
  - [ ] parent transcript remains isolated
- [ ] Add persistence tests proving:
  - [ ] child sessions are persisted with `ParentID`
  - [ ] child sessions are excluded from root session lists
  - [ ] child sessions can be discovered from the parent
- [ ] Add integration tests for nested delegation and multi-turn continuation.

### Files
- [ ] `pkg/runtime/delegation/manager_test.go`
- [ ] `pkg/runtime/agent_delegation_handler_test.go`
- [ ] `pkg/runtime/delegation_integration_test.go`
- [ ] `pkg/tools/builtin/delegate_test.go`
- [ ] `pkg/session/store_test.go`

---

## Cleanup / Deletions

- [ ] Remove or stop using `transfer_task` runtime/tooling.
- [ ] Remove or stop using background-agent runtime/tooling.
- [ ] Remove synthetic task-wrapper prompt/session helpers.
- [ ] Remove dead event types and dead compatibility code.

---

## Validation

- [ ] `go build ./...`
- [ ] `go test ./pkg/runtime/... ./pkg/runtime/delegation/... ./pkg/tools/builtin/... ./pkg/session/... ./pkg/teamloader/...`
- [ ] Reviewer pass completed
- [ ] All work remains local to `better-subagents`
