# Unified Delegation / Subagent Redesign Plan

**Branch:** `better-subagents` (worktree: `.worktrees/better-subagents`)  
**Base:** `upstream/main`  
**Date:** 2026-04-04

---

## Objectives

- [ ] Replace the fragmented `handoff`, `transfer_task`, and `background_agents` mental models with a unified delegation subsystem.
- [ ] Make subagents background-capable by default without any special YAML toolset or separate background-agent config.
- [ ] Preserve a clean UX: delegations are visible minimally in the TUI, with an explicit delegation tree and optional jump-in controls.
- [ ] Eliminate polling as the primary parent/subagent coordination mechanism.
- [ ] Support nested delegation, cancellation, steering, interruption, and parent auto-resume on child completion.
- [ ] Keep backward compatibility where possible through migration/aliases while redesigning internals for reliability.
- [ ] Add comprehensive tests across runtime, persistence, API, and TUI behavior.

---

## Phase 0 — Baseline, research, and guardrails

- [ ] Inspect current delegation-related code paths:
  - `pkg/runtime/agent_delegation.go`
  - `pkg/tools/builtin/agent/agent.go`
  - `pkg/tools/builtin/transfertask.go`
  - `pkg/tools/builtin/handoff.go`
  - `pkg/runtime/event.go`
  - `pkg/runtime/runtime.go`
  - `pkg/runtime/persistent_runtime.go`
  - `pkg/session/*`
  - `pkg/tui/page/chat/*`
  - `pkg/tui/components/tool/*`
  - `pkg/tui/components/sidebar/*`
- [ ] Review the `background-agent-fixes` branch and explicitly carry forward its bug fixes / race-condition learnings.
- [ ] Create/expand regression tests for currently known background-agent and sub-session bugs before major refactors.
- [ ] Keep all work confined to `/Users/krissetto/dev/ai-dev/docker-agent/.worktrees/better-subagents`.

---

## Phase 1 — Unified delegation domain model

- [ ] Introduce a first-class delegation domain instead of split concepts.
- [ ] Define a canonical delegation lifecycle/state model, including:
  - [ ] pending
  - [ ] running
  - [ ] waiting-for-parent / awaiting-steering
  - [ ] completed
  - [ ] failed
  - [ ] cancelled
  - [ ] superseded / abandoned
- [ ] Define clear parent/child semantics:
  - [ ] parent agent delegates task to child agent
  - [ ] child communicates primarily to parent, not directly into root transcript
  - [ ] nested child delegations are allowed only if configured via subagents
- [ ] Establish a stable `DelegationID` / task identity model for runtime, persistence, and TUI/API navigation.
- [ ] Decide how legacy concepts map to the new model:
  - [ ] `transfer_task` => synchronous delegation request
  - [ ] `handoff` => conversation-control delegation / active agent transfer variant
  - [ ] `run_background_agent` => async delegation alias over the same engine

---

## Phase 2 — Runtime orchestration redesign

- [ ] Extract or introduce a dedicated delegation manager/orchestrator in the runtime layer.
- [ ] Unify synchronous and asynchronous subagent execution under one engine rather than separate code paths.
- [ ] Replace polling-based coordination with event-driven parent/child signaling.
- [ ] Ensure the parent runtime can:
  - [ ] wait synchronously for a child result
  - [ ] continue other work while the child runs in background
  - [ ] be resumed automatically when a child finishes
  - [ ] cancel or supersede children when user intent changes
- [ ] Ensure child runtime can:
  - [ ] stream to parent/orchestrator channels
  - [ ] be steered mid-flight by parent or user
  - [ ] delegate further to its own configured subagents
- [ ] Preserve or improve isolation guarantees for background work (including cancellation boundaries and session pinning).
- [ ] Incorporate `background-agent-fixes` lessons directly into the new orchestration layer:
  - [ ] no goroutine leaks
  - [ ] no duplicate sub-session persistence linkage
  - [ ] race-safe stop/start semantics
  - [ ] correct output truncation/capping
  - [ ] correct parent materialization/persistence ordering

---

## Phase 3 — Public tool and runtime API shape

- [ ] Design a canonical tool/API surface for delegation.
- [ ] Decide whether to add a new core tool (for example a unified `delegate`) while preserving old tool names as compatibility aliases.
- [ ] Preserve backward compatibility for existing configs and prompts as much as possible.
- [ ] Provide runtime/API operations to:
  - [ ] list delegations for a session
  - [ ] inspect delegation status and latest output
  - [ ] stop/cancel a delegation
  - [ ] steer/send a message to a delegation
  - [ ] jump/focus into a delegated session
- [ ] Ensure the parent can be proactively resumed without requiring polling tools.
- [ ] Keep old background-agent tools functional as wrappers if needed, but ensure the internals use the unified delegation subsystem.

---

## Phase 4 — Config and schema migration strategy

- [ ] Update **latest config only**; older config packages remain frozen.
- [ ] Design the migration strategy for existing fields:
  - [ ] `sub_agents`
  - [ ] `handoffs`
  - [ ] `background_agents` toolset references
- [ ] Make subagent background capability implicit: defining a subagent should be enough.
- [ ] Remove the need for any special YAML tool declaration for background behavior in the new model.
- [ ] Decide whether `handoffs` remains as deprecated compatibility syntax, maps into unified delegates, or is partially retained for special semantics.
- [ ] Update:
  - [ ] `pkg/config/latest/types.go`
  - [ ] `pkg/config/config.go`
  - [ ] `pkg/teamloader/teamloader.go`
  - [ ] `pkg/teamloader/registry.go`
  - [ ] `agent-schema.json`
- [ ] Add/update an example YAML demonstrating the new delegation model.

---

## Phase 5 — Persistence and session model

- [ ] Extend persistence so delegation state is durable and reconstructable.
- [ ] Ensure every child session/delegation stores enough metadata to rebuild the delegation tree.
- [ ] Ensure nested delegations restore correctly across app restart/session reload.
- [ ] Support resumed parent execution after child completion without transcript corruption.
- [ ] Keep child transcript distinct from parent transcript while maintaining inspectability.
- [ ] Audit and improve session linkage logic around:
  - [ ] parent session IDs
  - [ ] child session IDs
  - [ ] delegation IDs
  - [ ] active agent / focus session
  - [ ] persistence ordering guarantees
- [ ] Prevent the historical issues where subagent streaming or session restoration polluted the parent conversation.

---

## Phase 6 — TUI redesign for top-tier delegation UX

- [ ] Design and implement a delegation-focused TUI experience that is minimal by default.
- [ ] Add a dedicated delegation tree/session-navigation component.
- [ ] Ensure the main transcript shows delegation activity cleanly without dumping all child streaming output into the root chat.
- [ ] Allow the user to:
  - [ ] see when a delegation starts
  - [ ] see its state/progress minimally
  - [ ] inspect the delegation tree for the session
  - [ ] jump into a child session
  - [ ] send steering messages to a child session
  - [ ] stop/cancel child sessions
  - [ ] return back to the parent/root session cleanly
- [ ] Ensure the sidebar/session browser/focus model works well with nested delegations.
- [ ] Avoid the old UX confusion caused by all subagent streams appearing like root-agent output.
- [ ] Update likely TUI surfaces:
  - [ ] `pkg/tui/page/chat/runtime_events.go`
  - [ ] `pkg/tui/page/chat/chat.go`
  - [ ] `pkg/tui/components/messages/messages.go`
  - [ ] `pkg/tui/components/tool/factory.go`
  - [ ] existing transfer/handoff tool components
  - [ ] sidebar/session-state components
- [ ] Add or update a dedicated tool/component for the new delegation representation if needed.

---

## Phase 7 — Parent interruption, steering, and supersession

- [ ] Define clear semantics for what happens when the user changes direction while children are still running.
- [ ] Ensure parent agent can identify when prior child work is now obsolete.
- [ ] Implement runtime support so the parent can:
  - [ ] cancel one child
  - [ ] cancel many children
  - [ ] supersede previous tasks with a replacement task
  - [ ] steer an existing child instead of recreating it
- [ ] Ensure cancellation and steering semantics are safe for nested delegation chains.
- [ ] Make interruption behavior deterministic and thoroughly tested.

---

## Phase 8 — Testing matrix

- [ ] Add unit tests for delegation manager/state machine behavior.
- [ ] Add runtime integration tests for:
  - [ ] sync wait semantics
  - [ ] async continue semantics
  - [ ] parent auto-resume after child completion
  - [ ] nested delegation
  - [ ] cancellation
  - [ ] steering
  - [ ] supersession
  - [ ] session restore
  - [ ] transcript isolation
- [ ] Add persistence tests for child linkage and delegation-tree reconstruction.
- [ ] Add regression tests specifically covering bugs/edge cases from `background-agent-fixes`.
- [ ] Add TUI tests for delegation tree rendering, focus switching, and clean root transcript behavior.
- [ ] Add API/runtime tests for jump-in / inspect / stop / steer operations.
- [ ] Run relevant validation commands (`mise test`, targeted packages, and if feasible lint/build).

---

## Likely code areas to modify

- [ ] `pkg/runtime/agent_delegation.go`
- [ ] `pkg/runtime/runtime.go`
- [ ] `pkg/runtime/event.go`
- [ ] `pkg/runtime/persistent_runtime.go`
- [ ] `pkg/runtime/tool_dispatch.go`
- [ ] `pkg/runtime/loop.go`
- [ ] `pkg/tools/builtin/agent/agent.go`
- [ ] `pkg/tools/builtin/transfertask.go`
- [ ] `pkg/tools/builtin/handoff.go`
- [ ] `pkg/session/session.go`
- [ ] `pkg/session/store.go`
- [ ] `pkg/session/migrations.go` (if persistence schema needs extension)
- [ ] `pkg/config/latest/types.go`
- [ ] `pkg/config/config.go`
- [ ] `pkg/teamloader/teamloader.go`
- [ ] `pkg/teamloader/registry.go`
- [ ] `pkg/tui/page/chat/*`
- [ ] `pkg/tui/components/messages/*`
- [ ] `pkg/tui/components/tool/*`
- [ ] `pkg/tui/components/sidebar/*`
- [ ] `pkg/app/*`
- [ ] `pkg/api/*` and/or runtime API exposure points
- [ ] `agent-schema.json`
- [ ] examples/docs/tests as needed

---

## Risks and explicit design checks

- [ ] Avoid recreating old background-agent race conditions and process-lifetime bugs.
- [ ] Avoid transcript corruption from child streaming events leaking into parent/root history.
- [ ] Avoid ambiguous ownership of cancellations across parent/child runtime contexts.
- [ ] Avoid hidden delegation state that the TUI cannot explain to the user.
- [ ] Avoid breaking existing multi-agent configs without a clear compatibility path.
- [ ] Ensure the redesign works for both local models and frontier hosted models, with deterministic runtime behavior.

---

## Validation and signoff checklist

- [ ] Runtime design implemented and stable
- [ ] Config/schema updated in latest only
- [ ] TUI delegation tree and focus UX implemented
- [ ] Backward compatibility path verified
- [ ] Comprehensive tests added
- [ ] Reviewer pass completed
- [ ] Validation commands run and recorded
- [ ] Changes remain local to `better-subagents` worktree/branch
