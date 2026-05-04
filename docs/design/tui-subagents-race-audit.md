# TUI subagents – race / edge case audit

Investigating the new subagent code, specifically the TUI attach-to-sub-session
paths. Checklist for this session:

- [x] Map the code surface (supervisor, tui.go, app.App attached mode, chat page, subagent manager, live sessions)
- [x] Baseline: run existing tests — all green
- [x] Run existing tests with `-race`
- [x] Audit concrete race / edge-case seams
  - [x] Supervisor.AddSession called twice with the same session id (order slice / cancel leak)
  - [x] Supervisor.AttachSession dedup vs. concurrent attach of the same node
  - [x] Supervisor.AttachSession with an already-cancelled ctx (no goroutine leak)
  - [x] Supervisor.CloseSession racing with in-flight subscribe goroutine
  - [x] Supervisor.CloseSession cleanup runs off the critical path (cannot deadlock)
  - [x] Shutdown releases attached subscriptions even when program never attached
  - [x] Subscribe goroutine honours `programReady` gate even on ctx cancel during wait
  - [x] Events emitted before `programReady` are not silently dropped
  - [x] Handle rapid stream-started/stopped cycles without runner state churn
  - [x] Tab metadata (`Kind`, `ParentSessionID`, etc.) survives a title update
  - [x] `app.InterruptAttachedSession` rejects owned apps even after a session.id is set
  - [x] Cascading close (attached sibling survives parent close) — already covered but re-check semantics
- [x] Add focused tests for the seams above
- [x] Fix anything that falls out
- [x] Validate: `go test -race` for affected packages
- [x] Validate: broad `go test ./...`
- [x] Summarize
