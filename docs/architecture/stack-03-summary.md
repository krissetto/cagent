# Stack 03: Runtime-Managed Subagent Sessions

## Local stack identity

- Branch: `local/stack-03-runtime-subagents`
- Worktree: `../docker-agent-stack-03-subagents`
- Base: `local/stack-02-durable-public-events` (`PR2`) at the time this branch was created
- Stack position: PR3; parent is Stack 02 and child is `local/stack-04-tui-runtime-subagents`
- Push/upstream/PR status: local only; no upstream configured and no PR created

## Scope owned by this stack

This branch owns runtime-managed subagent session lifecycle and routing on top of
Phase 1 durable topology and Stack 02 durable public event stream contracts. The
intended implementation boundary is:

- create runtime-managed child sessions through the Phase 1 child-session
  constructor/topology path;
- route parent-to-child and child-to-parent updates through runtime session
  ownership boundaries;
- coordinate background runtime-managed subagent sessions, including nested
  subagents, without TUI-specific behavior;
- emit durable public events using Stack 02 contracts once they exist;
- add runtime/session tests for direct and nested managed children, cancellation,
  completion, and replay/routing assumptions.

This branch intentionally does **not** own TUI tree rendering, sidebar UX,
transcript presentation, or redesign of Stack 02 event schemas/storage.

## Current status

Scaffold / handoff only. No production code has been changed on this branch yet.
This keeps the stacked branch available above PR2 while avoiding speculative
runtime lifecycle changes before the durable event stream contract lands.

## Proposed implementation checklist

1. Identify the runtime entry point that creates subagent sessions and ensure all
   managed children use the Phase 1 topology helper.
2. Introduce a small runtime-owned registry/manager for live managed children if
   one is needed, keeping durable identity in the session store rather than in
   transient UI structures.
3. Connect child session status/update publication to Stack 02 event append APIs
   after PR2 defines them.
4. Add tests for parent session isolation, direct child completion, nested child
   routing, cancellation/stop behavior, and restart/replay expectations.
5. Keep all TUI wiring out of this branch; expose runtime APIs/events that Stack
   04 can consume.

## Validation performed for this scaffold

- `git status --short --branch`
- `git branch -vv --list local/stack-03-runtime-subagents`
- `git diff --check`

No Go tests were required for this scaffold because only this documentation file
was added.

## Known gaps / risks

- Runtime-managed child lifecycle/routing is not implemented yet.
- This branch depends on Stack 02 event contracts that are currently scaffolded
  only.
- Care will be needed to avoid duplicating existing agent delegation paths or
  creating parallel, inconsistent session ownership models.

## Restack notes

If Stack 02 changes, restack this branch onto the new
`local/stack-02-durable-public-events` HEAD before restacking Stack 04. Keep any
runtime lifecycle commits above Stack 02 only, and do not fold TUI work into this
branch.
