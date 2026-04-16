# Better Subagents — Remaining Work Plan

Branch: `better-subagents` (rebased on main)
Last validated: `go build ./...` ✅ | all tests ✅

---

## CRITICAL GAPS (must fix)

### GAP 1: Notification content is TOO VERBOSE — full reply in parent session ❌
Current: `[Delegation ab3k9 completed] Agent 'coder' finished with result: <FULL REPLY>`
Required: `coder (ab3k9) has responded` — short notification only
The full reply must NOT end up in the parent session. Two sessions must remain distinct.

### GAP 2: Notification goes through user message queue (wrong queue) ❌
Current: `DelegationResumeMsg` → `pendingDelegationResumes` → drains on StreamStopped
Required: Route via `steerQueue` (drained at top of each iteration = earliest safe moment)
When parent is IDLE: TUI still auto-restarts (via onDelegationEvent path)
When parent is RUNNING: steerQueue delivers at earliest safe moment (after current tool calls)

### GAP 3: No way for parent to retrieve child result ❌
With short notification only, parent needs a tool to get the result.
Need: `get_delegation_result(delegation_id)` — synchronous read of d.GetLastReply()

### GAP 4: Child session title not generated ❌
Required: Generate title after first parent-agent message (first model response in child)
Current: handleOpenChildSession derives a raw truncated title from d.Task (not generated)
Fix: In RunDelegation, after stream completes, if title empty → derive from first user msg →
     publish SessionTitleEvent to EventBus → child tab updates

### GAP 5: Esc in child tab does not cancel child delegation ❌
Current: Esc in child tab would call app.Cancel() (wrong — cancels parent or nothing)
Required: Esc → Manager.Stop(delegationID) → DelegationStoppedEvent → no parent auto-resume

### GAP 6: System prompt not updated for new interaction model ❌
Required: Explain notifications format, get_delegation_result tool, async model

---

## Completed ✅
- Parallel async delegations (Manager.Start launches goroutines)
- continue_delegation is async (fans out N subagents without blocking)
- Child session tab with spinner + live streaming across turns (WatchReopens)
- User messages in child tab render as normal user bubbles
- User can send message from child tab (→ Manager.Continue)
- Subagent pills in sidebar, not messages
- Working directory inheritance
- Correct sub-agent resolution (resolveSessionAgent searches SubAgent trees)
- Parent auto-resume when idle (via onDelegationEvent → TUI)
- Separate pending queue (pendingDelegationResumes) from user messageQueue

---

## Implementation order
1. QueuedMessage.Kind for steer formatting differentiation
2. handleDelegationCompletion: enqueue to steerQueue when running, short content
3. runtime_events.go: DelegationCompleted/Failed → short notification, only restart when idle
4. RunDelegation: title generation via EventBus after first run
5. Add get_delegation_result tool
6. Esc intercept for child tabs
7. System prompt update
8. Delegation struct: ensure Task field exists and propagates
