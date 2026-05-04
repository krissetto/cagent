# Subagents TUI follow-up fixes

Three issues the user reported after the first UX pass:

1. **Sidebar spins the parent agent row while parent is idle-waiting on subagents.**
   When `ParentIdleEvent` fires, the sidebar stops the global working
   spinner, but the per-agent row in the Agents list still shows
   `m.spinner.RawFrame()` because its condition is only
   `m.workingAgent == agent.Name`. It needs to also check `!m.parentIdle`.

2. **Subagent response previews are leaked into the UI.**
   When a child subagent finishes a turn, the runtime emits
   `SubAgentUpdateEvent{kind: turn_completed, preview: "..."}`. The chat
   page turns that into a `MessageTypeSubAgent` card whose body renders the
   preview text. The sidebar also renders the preview as a second line
   under each subagent row. User wants that content hidden.
   - Transcript: suppress `TurnCompleted` cards entirely (terminal
     Closed/Stopped/Failed cards remain — they carry no response text
     except the error for Failed, which is useful).
   - Sidebar: stop rendering the `Preview` line, except for `Failed`
     rows where the preview is actually the error string.

3. **Subagent pills inside tool-call messages are not clickable.**
   The compact pills rendered by `pkg/tui/components/tool/subagent` already
   carry a short ref (and for start, an agent-name badge), but clicking
   them does nothing. The sidebar already routes row clicks to
   `OpenSubAgentTabMsg`; we want the pill click to do the same.

## Implementation sketch

### Fix 1 — sidebar parent row
`pkg/tui/components/sidebar/sidebar.go :: renderAgentEntry`
```go
if isCurrent {
    if m.workingAgent == agent.Name && !m.parentIdle {
        prefix = agentStyle.Render(m.spinner.RawFrame()) + " "
    } else {
        prefix = agentStyle.Render("▶") + " "
    }
}
```

Test in `sidebar` package: push a `ParentIdleEvent`, render the agent
section, assert the spinner frame is absent and the static `▶` glyph is
present for the working agent row. Then push `ParentResumeEvent`, assert
the spinner frame returns.

### Fix 2 — no response previews
`pkg/tui/page/chat/runtime_events.go :: handleSubAgentUpdate`
- On `SubAgentEventTurnCompleted`, still forward to the sidebar and
  scroll, but do NOT call `AddSubAgentMessage`.
- Keep the card for Closed / Stopped / Failed (their detail is status
  text or error).

`pkg/tui/components/sidebar/subagents.go :: renderSubagentEntry`
- Only render the preview line when `s.Status == subagent.StatusFailed`.

Update the two existing sidebar tests that assert on the preview:
- `TestSubagents_UpdateAppliesPreviewAndStatus` — keep the state-level
  assertion on `state.Preview`, but flip the render assertion to verify
  the preview is NOT visible.
- `TestSubagents_FailedUsesErrorAsPreview` — still asserts the error is
  visible.

Add a new chat-page test covering the `handleSubAgentUpdate` branch:
`TurnCompleted` must not enqueue a `MessageTypeSubAgent` card; `Closed`
still must.

### Fix 3 — clickable tool-call pills
`pkg/tui/components/tool/subagent/subagent.go`
- Add an accessor `SubAgentShortRef() string` that returns the short ref
  associated with the tool call, derived from:
  - completed `subagent_start`: parse `msg.Content` JSON for
    `subagent_id`.
  - `subagent_send` / `_inspect` / `_finalize` / `_close` / `_stop`:
    parse args for `subagent_id`.
  - otherwise "".

`pkg/tui/messages/agent.go`
- Add `OpenSubAgentByShortRefMsg{ShortRef string}`.

`pkg/tui/components/messages/messages.go :: handleMouseClick`
- After the existing edit-label / copy-label checks, if the clicked
  message is a `MessageTypeToolCall` whose view implements
  `SubAgentShortRef() string`, emit
  `OpenSubAgentByShortRefMsg{ShortRef: ref}` when ref != "". No column
  check — the whole body line is about this one subagent, same
  single-click-anywhere behaviour as the sidebar rows.

`pkg/tui/page/chat/chat.go :: Update`
- Handle `OpenSubAgentByShortRefMsg` by resolving the short ref via
  `p.app.LiveSessionTree(rootID)` and emitting
  `OpenSubAgentTabMsg{SessionID: fullID}`. If no live node matches,
  surface a muted notification.

`pkg/app/app.go`
- Add `LiveSessionTree(rootID string) []runtime.LiveSessionNode` as a
  thin passthrough to `SessionTreeProvider`.

Tests:
- `subagent_test.go`: assert `SubAgentShortRef()` returns the expected
  value for each variant (args-sourced and content-sourced).
- `chat_test.go` or a new file: with an `attachedTestRuntime`-style fake,
  dispatch `OpenSubAgentByShortRefMsg{ShortRef: "abcde"}` and verify the
  resulting command contains `OpenSubAgentTabMsg{SessionID: "abcdef..."}`.

## Progress

- [x] Explore & understand relevant code
- [x] Fix 1 — sidebar parent-row spinner + test
- [x] Fix 2 — suppress response previews (transcript + sidebar) + tests
- [x] Fix 3 — clickable tool-call pills + tests
- [x] Run targeted `go test -race` + full `go test ./...`
- [x] Review + summarize

## Validation summary

- `go build ./...` is clean.
- `go test ./...` passes (full tree).
- `go test -race ./pkg/tui/components/sidebar/... ./pkg/tui/components/tool/subagent/... ./pkg/tui/components/messages/... ./pkg/tui/page/chat/... ./pkg/tui/messages/... ./pkg/app/...` is green — this covers every package touched in this session plus their integration surface.
- `go test -race ./...` exposed one flaky failure in `pkg/runtime/TestLocalRuntime_SubscribeSessionReceivesChildObserverEvents`. Reproduced on the baseline with my changes stashed (~1 in 5 runs), so it is a **pre-existing** race in `pkg/runtime/streaming.go` (unlocked writes to `sess.InputTokens/OutputTokens` racing a locked read during subagent title generation). Not introduced by this slice; not in scope here.
