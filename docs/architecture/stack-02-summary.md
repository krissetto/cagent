# Stack 02: Durable Public Runtime Event Stream

## Local stack identity

- Branch: `local/stack-02-durable-public-events`
- Worktree: `../docker-agent-stack-02-events`
- Base: `phase1-subagent-runtime-topology` (`PR1`) at the time this branch was created
- Stack position: PR2; parent is PR1 and child is `local/stack-03-runtime-subagents`
- Push/upstream/PR status: local only; no upstream configured and no PR created

## Scope owned by this stack

This branch owns the durable public event stream layer that future runtime-managed
subagent sessions and TUI integration can consume. The intended implementation
boundary is:

- public runtime event envelope contracts;
- durable storage for append-only runtime events;
- ordered replay by session/root/session-tree;
- migration/backfill strategy for any new storage objects;
- targeted tests for ordering, persistence, and replay semantics.

This branch intentionally does **not** own runtime-managed subagent lifecycle,
routing, background child session orchestration, or TUI/UX changes. Those are
reserved for stacks 03 and 04.

## Current status

Scaffold / handoff only. No production code has been changed on this branch yet.
This was kept intentionally narrow so the local stacked worktrees are usable for
planning and testing without introducing risky broad persistence or protocol
changes before PR2 design review.

## Proposed implementation checklist

1. Define the public event envelope near existing runtime event contracts, using
   Phase 1 topology fields (`session_id`, `root_id`, and `parent_id`) without
   redesigning lifecycle behavior.
2. Add a session-store-backed append/replay interface with deterministic sequence
   assignment scoped to the appropriate stream.
3. Add SQLite migration(s) and in-memory parity for tests.
4. Add focused replay tests for direct sessions, child sessions, nested sessions,
   event ordering, and restart persistence.
5. Keep remote/TUI changes out of this branch unless strictly necessary to expose
   the durable event stream contract.

## Validation performed for this scaffold

- `git status --short --branch`
- `git branch -vv --list local/stack-02-durable-public-events`
- `git diff --check`

No Go tests were required for this scaffold because only this documentation file
was added.

## Known gaps / risks

- Durable event schema and sequence semantics are not implemented yet.
- Replay API shape is not finalized.
- Future code should avoid coupling the event stream to Stack 03 lifecycle
  routing or Stack 04 TUI rendering.

## Restack notes

If PR1 changes, restack this branch onto the new `phase1-subagent-runtime-topology`
HEAD before restacking Stack 03 and Stack 04. After code is added here, create a
commit on this branch before creating or rebasing upper stack branches so Stack
03 remains based on this branch's HEAD.
