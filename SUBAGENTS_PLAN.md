# Better Subagents — Remaining Work Plan

Branch: `better-subagents` (from `b5a564de`)  
Last validated: `go build ./...` ✅ | `go test -race ./pkg/runtime/... ./pkg/tui/page/chat/... ./pkg/app/...` ✅

---

## Status legend
- ✅ Done + tested
- 🔄 In progress
- ❌ Not started
- ⏸️ Deferred (post-MVP)

---

## Completed

- ✅ Build blockers removed; delegation lifecycle races fixed (Iter 1)
- ✅ RunDelegation pure executor; duplicate user-message bug fixed (Iter 2)
- ✅ Session-scoped EventBus; incremental child session persistence (Iter 3)
- ✅ Delegation eviction/TTL; accurate tool descriptions; Continue queue semantics (Iter 4)
- ✅ Correct sub-agent resolution (`resolveSessionAgent` searches SubAgent trees)
- ✅ Parent-agent messages (task + continue) persisted and visible in child session
- ✅ Delegation pills moved to sidebar (with status icons + click-to-open)
- ✅ Live streaming in child tab persists across all continue_delegation turns (WatchReopens)
- ✅ "delegation is already running" false-positive fixed (Continue now waits, not rejects)
- ✅ continue_delegation fully async (parent fans out to N subagents without blocking)
- ✅ Parent auto-resume: when child finishes, parent session auto-triggered via DelegationResumeMsg
- ✅ Subagent completions bypass user message queue (dedicated pendingDelegationResumes)
- ✅ Parent transcript shows compact `[agentName] responded` instead of full user bubble
- ✅ Working directory inherited by child sessions from parent
- ✅ Title generation triggered for parent session on first delegation resume

---

## Remaining — Priority Order

### ITEM 1: Child session spinner + title on tab open  ✅
**Fixed**: `handleOpenChildSession` now:
- Sends synthetic `StreamStartedEvent` if delegation is `StatusRunning` → primes spinner
- Derives a short title from `Delegation.Task` (first 60 chars) if `sess.Title == ""`
- `OpenChildSessionMsg` carries `DelegationID` for status lookups
- `App.DelegationManager()` accessor added for TUI-layer access to Manager

---

### ITEM 2: Child session "user is parent agent" parity  ✅
**Verified correct**: Parent-agent task/follow-up messages are stored as `session.UserMessage`
(`Implicit: false`, `IsSubagentResult: false`) — they render as normal user bubbles in the
child tab. No changes needed.

---

### ITEM 3: User intervention in child session  ✅
**Fixed**:
- `childTabDelegationIDs map[string]string` tracks which tabs are child delegation sessions
- When user sends a message from a child tab, tui.go intercepts `SendMsg` and:
  1. Calls `Manager.Continue(ctx, delegationID, content)` (async)
  2. Forwards a `ChildTabSendMsg` to the chat page for display-only (user bubble + spinner)
  3. Does NOT call `app.Run()` (avoids duplicate execution)
- `ChildTabSendMsg` type added; chat page handles it via `handleChildTabSendMsg`
- The child agent processes the message, completion flows to parent via `onDelegationEvent`
- Tab close cleans up both `childBusSubs` and `childTabDelegationIDs`

---

### ITEM 4: eventbus.go getEventType coverage  ⏸️
**Problem**: `getEventType` in eventbus.go is missing delegation event types (log quality only).
Low severity — deferred.

---

### ITEM 5: handleOpenChildSession double initSessionComponents  ⏸️
**Problem**: `editor` is not `Cleanup()`'d before `initSessionComponents` is called a second
time. Pre-existing pattern, low severity. Deferred.

---

## Invariants to preserve

1. `go build ./...` must be clean after every commit
2. `go test -race ./pkg/runtime/... ./pkg/tui/page/chat/... ./pkg/app/...` must pass
3. No double-delivery of delegation completion events (single path: onDelegationEvent)
4. User message queue is separate from delegation resume queue
5. Only terminal delegations are evicted from Manager; running delegations are never evicted
6. `continue_delegation` is async; parent can fan out to N subagents without blocking
7. Child sessions correctly run as the target sub-agent (resolveSessionAgent sub-tree search)
8. Parent auto-resumes via DelegationResumeMsg when any child completes
9. User-cancelled child delegations do NOT auto-resume parent
