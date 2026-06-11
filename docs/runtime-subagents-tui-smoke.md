# Runtime-managed subagents TUI smoke checklist

Use the final stacked `v3-pr4-tui` branch for maker manual end-to-end testing.

1. Build the binary from the worktree:

   ```sh
   task build
   ```

2. Start the TUI with a config that exposes runtime-managed subagents, for example:

   ```sh
   ./bin/docker-agent run --config ./examples/runtime-subagents.yaml
   ```

   If that exact example is not present in your checkout, use any local config whose root agent has at least two entries under `subagents:` (for example `greppy` and `reviewer`).
3. Ask the root agent to start two children and ask one child to start a nested child.
4. Confirm the sidebar shows a Subagents section with live status indicators, agent-colored labels, and preview text.
5. Run `/subagents`; confirm the live subagent tree dialog shows both children and the nested child.
6. Click a child in the sidebar or select it in `/subagents`; confirm it opens in an attached tab with the same working directory display as the root tab.
7. Send a message in the attached child tab while the child is idle; confirm it is delivered as a runtime follow-up to the child, not the root.
8. While a child is busy, send another message and confirm the child Queue section updates from `SessionQueueEvent`.
9. While the parent is waiting on children, send root input and confirm the spinner turns off, input remains active, and queued input flushes on `ParentIdleEvent`.
10. Stop one child and finalize another using the runtime subagent tools; confirm sidebar/tree status updates.
11. Kill and restore the TUI session; confirm root session restoration still works and live attachments fail gracefully if no longer live.
