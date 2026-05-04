# TUI plan for runtime-managed subagents

Goal: make runtime-managed subagents feel like a first-class part of the TUI, not a special-case bolt-on.

## User-facing requirements

- [ ] When an agent delegates to a background subagent, show that delegation:
  - [ ] in the message timeline
  - [ ] in the sidebar
- [ ] Let the user click a delegation in the sidebar and open a new tab attached to the live subagent session.
- [ ] The attached subagent tab should behave like a normal session tab:
  - [ ] live streamed messages
  - [ ] working spinners
  - [ ] tool calls / tool responses
  - [ ] delegations in the sidebar
  - [ ] correct active/working agent in the sidebar
  - [ ] interrupt / steer / follow-up support
- [ ] In subagent tabs, show the parent agent/session in the sidebar.
- [ ] Let the user click the parent indicator to jump back to the parent tab.

## Constraints / design principles

- [ ] Reuse the existing multi-tab + routed-event architecture where possible.
- [ ] Keep attached subagent tabs visually and behaviorally consistent with normal session tabs.
- [ ] Avoid creating a separate “mini chat” implementation for subagent tabs.
- [ ] Preserve backward compatibility with existing root-session flows.
- [ ] Keep each implementation slice testable and independently shippable.

## Architecture observations

These are already available and should be reused:

- [x] `pkg/tui/service/supervisor` already manages multiple tabs and routes events by `SessionID`.
- [x] `pkg/runtime.Client` / `RemoteClient` already expose live-session APIs:
  - [x] `GetLiveSessionTree`
  - [x] `GetLiveSession`
  - [x] `AttachLiveSession`
  - [x] `SteerLiveSession`
  - [x] `FollowUpLiveSession`
  - [x] `CloseLiveSession`
  - [x] `StopLiveSession`
- [x] Runtime already emits:
  - [x] `subagent_started`
  - [x] `subagent_sent`
  - [x] `subagent_update`
- [x] Sidebar already understands:
  - [x] working spinners
  - [x] current/working agent
  - [x] click zones for agents / title / working dir

Main gap: the TUI does not yet have a concept of an **attached live session tab** for descendant sessions.

## High-level design

### 1. Introduce attached session tabs

Add a new session-runner kind so the Supervisor can manage both:

- [ ] normal owned tabs (root sessions backed by `*app.App`)
- [ ] attached tabs (live descendant sessions backed by `RemoteClient.AttachLiveSession`)

Proposed additions to `SessionRunner`:

- [ ] `Kind SessionKind` (`Owned` / `Attached`)
- [ ] `ParentSessionID string`
- [ ] `RootSessionID string`
- [ ] `AgentName string`
- [ ] enough metadata to render parent/child relationships in the tab bar and sidebar

### 2. Add a session interaction abstraction

To avoid forking the chat page into “normal session” vs “attached session” modes, add a small interface that captures what the chat UI actually needs.

Proposed `SessionInteractor`:

- [ ] subscribe to events
- [ ] send a user message / steer
- [ ] queue a follow-up
- [ ] interrupt / stop
- [ ] resume confirmations / elicitation where supported
- [ ] expose `SessionID()` / current agent identity

Implementations:

- [ ] adapter for existing `*app.App`
- [ ] new `AttachedSession` implementation backed by `RemoteClient`

### 3. Sidebar support for subagent trees

Extend the sidebar with two new ideas:

#### Parent indicator

For attached subagent tabs:

- [ ] show a parent row, e.g. `↑ parent-agent`
- [ ] clicking it jumps back to the parent tab

#### Subagents section

For any tab with children:

- [ ] show a `Subagents (N)` section
- [ ] show each child with:
  - [ ] status icon/spinner
  - [ ] agent name
  - [ ] preview / summary
- [ ] clicking a child row opens or focuses the attached child tab

### 4. Message timeline delegation cards

In the main transcript, represent delegation as a first-class message card:

- [ ] `SubAgentStartedEvent` renders a “delegated to X” card
- [ ] `SubAgentUpdateEvent` updates that card with status + preview
- [ ] clicking the card opens/focuses the attached child tab

This lets delegation be visible both in the transcript and the sidebar.

### 5. Attached tab transcript behavior

An attached subagent tab should show the child session as if it were a normal chat tab:

- [ ] historical transcript replay when the tab opens
- [ ] live streamed assistant output
- [ ] tool calls and tool responses
- [ ] parent→child injected messages as visible `user_message` events
- [ ] descendant-triggered wake-ups causing new turns to appear naturally

### 6. Steering / interrupt semantics

Attached tabs should support user control over live subagents:

- [ ] send user input to the live subagent session
- [ ] interrupt / stop the live subagent session
- [ ] possibly close gracefully
- [ ] decide how attached tabs should behave for confirmation/elicitation flows

Current proposed behavior:

- [ ] `send` → `SteerLiveSession` or `FollowUpLiveSession` depending on session state
- [ ] `interrupt` → `StopLiveSession`
- [ ] `close` available as explicit action
- [ ] tool confirmations remain owned by the parent/root workflow unless we later add explicit remote approval support

## Implementation slices

### Slice 1 — Supervisor support for attached tabs

- [ ] Add `SessionKind` to `SessionRunner`
- [ ] Extend `TabInfo` with parent/agent metadata
- [ ] Add `AttachSession(ctx, parentRunner, liveNode)` to Supervisor
- [ ] Route `AttachLiveSession` SSE events into the existing `messages.RoutedMsg` path
- [ ] Keep tab close semantics non-destructive (closing an attached tab drops the subscription, not the child session)
- [ ] Add unit tests for attached-runner lifecycle and tab switching

### Slice 2 — SessionInteractor abstraction

- [ ] Introduce `SessionInteractor`
- [ ] Add adapter for `*app.App`
- [ ] Implement `AttachedSession` using `RemoteClient`
- [ ] Make the chat page consume the interface rather than concrete app state
- [ ] Add tests for attached-session subscribe/send/stop behavior

### Slice 3 — Sidebar tree support

- [ ] Add parent row rendering and click handling
- [ ] Add `Subagents (N)` section rendering
- [ ] Add click zones for child session rows
- [ ] Add sidebar state methods for adding/updating/removing child rows
- [ ] Wire `subagent_started` / `subagent_update` into sidebar state updates
- [ ] Add sidebar tests for click behavior and rendering

### Slice 4 — Delegation cards in transcript

- [ ] Add a transcript component for delegation cards
- [ ] Match `SubAgentStartedEvent` and `SubAgentUpdateEvent` by `subagent_id`
- [ ] Show status transitions and preview updates
- [ ] Clicking the card opens/focuses the child tab
- [ ] Add rendering tests

### Slice 5 — Transcript replay + attached-tab initialization

When opening an attached tab:

- [ ] fetch node metadata via `GetLiveSession`
- [ ] fetch historical transcript via `GetSession` if available
- [ ] synthesize whatever initial sidebar events are needed (`agent_info`, `team_info`, `toolset_info`) if not already available from the server
- [ ] then start live SSE attach
- [ ] ensure tab opens in a fully initialized, “normal-looking” state

### Slice 6 — UX polish and control actions

- [ ] add explicit stop/close actions for attached tabs
- [ ] define how attached tabs show terminal state (`closed`, `stopped`, `failed`)
- [ ] define tab badges / colors for child tabs
- [ ] optionally show parent/child visual grouping in the tab bar
- [ ] ring attention bell when attached tabs hit interruption-worthy states

### Slice 7 — Documentation and polish

- [ ] document subagent session tabs in user docs
- [ ] document sidebar parent/child navigation
- [ ] document interrupt/steer semantics for attached tabs

## Proposed state/data changes

### Supervisor / tab model

- [ ] `SessionRunner.Kind`
- [ ] `SessionRunner.ParentSessionID`
- [ ] `SessionRunner.RootSessionID`
- [ ] `SessionRunner.AgentName`
- [ ] `messages.TabInfo` extended with parent/kind/agent metadata

### Sidebar model

- [ ] add parent-session metadata
- [ ] add child-subagent row data structure
- [ ] new click results:
  - [ ] `ClickParentSession`
  - [ ] `ClickSubagentSession`

### New tea messages

Potential messages:

- [ ] `OpenAttachedSessionMsg{SessionID, ParentSessionID}`
- [ ] `FocusSessionMsg{SessionID}`
- [ ] `AttachedSessionOpenedMsg{SessionID}`
- [ ] `AttachedSessionFailedMsg{SessionID, Err}`

## Open design questions

These should be answered before or during Slice 2/3:

### Confirmations / elicitation in attached tabs

Question:

- [ ] Should a tool confirmation that originates from a subagent be actionable directly from the attached tab?

Current recommendation:

- [ ] Not in the first pass.
- [ ] Keep approvals owned by the existing root/session flow.
- [ ] If needed later, add explicit remote approval proxy support.

### Parent tab close semantics

Question:

- [ ] What happens if the parent tab is closed while attached child tabs remain open?

Current recommendation:

- [ ] Attached tabs remain open as live subscriptions until the session itself ends.
- [ ] They can still show/read/control the child via the live-session API.

### Transcript history for live-only child sessions

Question:

- [ ] Is `GetSession(childID)` always enough to replay a subagent transcript, or do we need a dedicated in-memory live-session transcript endpoint?

Action:

- [ ] Verify during Slice 5.
- [ ] If missing, add a small server endpoint rather than building a TUI-side special case.

### Tab bar hierarchy

Question:

- [ ] Should child tabs be visually grouped/indented in the tab bar?

Current recommendation:

- [ ] Yes, but only as a polish layer after the core flow works.
- [ ] Example: prefix child tabs with `└` or a subtle parent marker.

## Testing plan

### Unit tests

- [ ] Supervisor attached-tab lifecycle
- [ ] Supervisor tab switching across owned + attached tabs
- [ ] Sidebar parent row rendering + click behavior
- [ ] Sidebar subagent row rendering + click behavior
- [ ] Delegation card rendering + event matching

### Integration tests

- [ ] attached tab receives replayed transcript + live SSE events
- [ ] attached tab can steer a live subagent
- [ ] attached tab can stop a live subagent
- [ ] parent/child navigation works across tabs

### E2E-ish tests

- [ ] root delegates to child
- [ ] TUI opens child tab from sidebar or transcript card
- [ ] child tab shows messages, spinners, tool calls, and later parent messages
- [ ] user jumps back to parent from child sidebar

## Success criteria

- [ ] Delegations are clearly visible in both transcript and sidebar
- [ ] Clicking a delegation opens a live attached subagent tab
- [ ] Attached tabs feel like normal session tabs
- [ ] Parent/child navigation is easy and obvious
- [ ] Steering / interrupting a live subagent works from the attached tab
- [ ] No regressions in existing root-session tab behavior

## Recommended execution order

1. [ ] Slice 1 — Supervisor attached tabs
2. [ ] Slice 2 — SessionInteractor abstraction
3. [ ] Slice 3 — Sidebar parent/subagent tree support
4. [ ] Slice 4 — Delegation cards in transcript
5. [ ] Slice 5 — Replay + initialization polish
6. [ ] Slice 6 — UX control polish
7. [ ] Slice 7 — docs
