# Recursion hardening plan for runtime-managed subagents

Goal: move runtime-managed subagents from "recursion works structurally" to "recursion is proven, bounded, and safe under failure".

## Correctness properties we want to guarantee

- [x] a subagent can spawn its own subagents, to arbitrary depth (up to a configurable cap)
- [x] envelopes produced by a grandchild must propagate to its **direct** parent only, then walk up the chain via each parent's own normal loop behavior
- [x] a middle child whose grandchild produces an envelope must be woken up even if the middle child is currently idle (waiting for parent input) and even if the middle child's own RunStream has exited for lack of in-flight children
- [x] an explicit Stop on any ancestor must cascade down and stop all its descendants
- [x] a context cancellation on any ancestor must cascade down and stop all its descendants
- [x] depth is bounded by a configurable cap with a clear error to the tool caller
- [x] total descendants per root-session tree are bounded by a configurable cap with a clear error
- [x] each level has the same safe-time delivery semantics as the root
- [x] each level preserves the same isolation of per-loop coordination state (steer, follow-up, resume, elicitation) as root-vs-direct-child

## Design changes

### Manager
- [x] track each handle's direct parent id and depth
- [x] add `maxDepth` and `maxDescendants` (per root-ancestor tree) with sane defaults
- [x] add `ParentInboxSignal(sessionID) <-chan struct{}` so child loops can select on grandchild envelopes
- [x] add `CascadeStop(sessionID)` to stop all descendants of a given id
- [x] add `Depth(id)`, `Ancestors(id)`, `Descendants(rootID)` helpers for tests and observability
- [x] new typed errors: `DepthExceededError`, `DescendantLimitError`

### Handle
- [x] carry `depth`
- [x] expose it via `Snapshot`

### Runtime bridge
- [x] in the child loop's wait step, select also on the manager's parent-inbox signal for the child's own session id so a grandchild envelope wakes the middle child
- [x] drop `context.WithoutCancel` in `StartChild` so that cancel propagates down the tree cleanly

### Options
- [x] `NewManager(runner, opts ...ManagerOption)` with `WithMaxDepth`, `WithMaxDescendants`
- [x] defaults: depth=8, descendants=64

## Tests

### pkg/subagent (unit)
- [x] depth computed correctly at each level
- [x] `StartChild` rejects with `DepthExceededError` past the cap
- [x] `StartChild` rejects with `DescendantLimitError` when tree is full
- [x] `CascadeCancel` stops every live descendant
- [x] parent-inbox signal fires on publish
- [x] ancestor walk reaches the root session id

### pkg/runtime (integration, with mock providers)
- [x] `TestSubagentRecursion_ThreeLevels`: root → worker → leaf, full round-trip
  - [x] each level wakes its parent via safe-time envelope delivery
  - [x] root ends with a response composed after the grandchild's envelope propagated up the chain
- [x] `TestSubagentRecursion_MiddleChildWakesOnGrandchild`
  - [x] middle child reacts after grandchild envelope delivery
- [x] `TestSubagentRecursion_DepthCapEnforcedMidTree`
  - [x] maxDepth=1
  - [x] middle child tool-calls `subagent_start` for a grandchild
  - [x] call fails with a clear error from the runtime
- [x] `TestSubagentRecursion_CascadeStop`
  - [x] root stops the middle child
  - [x] grandchild ends up stopped too
- [x] `TestSubagentRecursion_ContextCancelCascades`
  - [x] parent ctx cancel kills every descendant loop

## Non-goals for this pass

- external API/client exposure for recursive tree browsing
- remote runtime parity for nested tree control
- legacy `run_background_agent` semantic changes (still uses `WithoutCancel`)

## Success criteria

- [x] every box above ticked
- [x] `go test ./...` still exits 0
- [x] `golangci-lint run` still reports 0 issues for touched packages
