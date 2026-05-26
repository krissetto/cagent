# Clean Subagent Runtime Architecture Plan

This plan defines a staged path toward a clean runtime-managed subagent architecture. The goal is to make subagents first-class runtime sessions without coupling the foundation to future UI or event-log work.

## Principles

- Preserve the current session and store interfaces unless a phase needs a small, reviewable extension.
- Keep topology metadata durable and queryable before adding richer replay or display features.
- Use one creation path for runtime-managed child sessions so direct children and nested subagents behave the same way.
- Avoid storing live runtime implementation details in durable session rows.
- Keep backward compatibility for existing sessions that do not have the new metadata.

## Phase 1: Durable session topology foundation

Implemented in this phase only:

- Add durable session metadata for new deployments:
  - `root_id`: identifies the root session for every session in a runtime tree.
  - `runtime_managed`: marks sessions created through the runtime-managed subagent path.
- Ensure root sessions have `root_id = id` when created or persisted by current code.
- Add a single child-session constructor for runtime-managed subagents. It derives the child `root_id` from the parent, sets `parent_id`, and marks the child runtime-managed.
- Keep existing parent/subsession behavior intact while making persisted topology explicit.
- Add minimal query helpers for topology:
  - list child sessions for a parent;
  - list all sessions in a root tree;
  - resolve any session id to its root id.
- Add tests for root, child, and nested topology across in-memory and SQLite stores.

Out of scope for Phase 1:

- Durable runtime event log or replay.
- TUI tree rendering, event streaming UI changes, or transcript UX redesign.
- Changing remote protocol contracts.
- Background job scheduling semantics beyond child session creation metadata.

## Phase 2: Runtime event envelope and durable event log

Future work:

- Define a stable runtime event envelope carrying event id, session id, root id, parent id, sequence, timestamp, and event type.
- Persist append-only runtime events separately from transcript/session rows.
- Backfill only what is safe from existing transcript items; avoid inventing missing runtime events.
- Add replay readers for tests and diagnostics without changing normal UI behavior initially.

## Phase 3: Runtime tree orchestration APIs

Future work:

- Add runtime-level APIs for starting, resuming, finalizing, and cancelling managed child sessions using Phase 1 topology and Phase 2 event envelopes.
- Ensure nested delegation is represented as a tree rooted by `root_id` and linked by `parent_id`.
- Define lifecycle states and state transitions for managed subagents.

## Phase 4: TUI and observer integration

Future work:

- Render runtime-managed child sessions from topology/event APIs instead of ad hoc transcript embedding.
- Surface child lifecycle, latest status, and completion summaries in the TUI.
- Keep transcript compatibility while progressively shifting live subagent display to the runtime tree.

## Phase 5: Migration hardening and compatibility cleanup

Future work:

- Add diagnostics for sessions missing topology metadata and safe repair tools where feasible.
- Document compatibility rules for old databases, remote runtimes, and exported sessions.
- Remove deprecated paths only after telemetry/review shows the new runtime APIs cover existing use cases.
