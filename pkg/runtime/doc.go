// Package runtime executes agent sessions: it owns the model/tool turn
// loop, live session event streaming, and the subagent control plane.
//
// # Key types
//
//   - [LocalRuntime] is the in-process implementation of [Runtime].
//     It embeds a shared [runtimeCore] (team, stores, event bus,
//     subagent manager) and a root-session [sessionState] (steer/follow-up
//     queues, resume channel, elicitation channel).
//
//   - [sessionRunner] is the first-class per-session driver. Root
//     sessions use a runner built from [LocalRuntime]; child sessions
//     use a runner built from the shared [runtimeCore] plus a fresh
//     [sessionState].
//
//   - [sessionEngine] drives a single live session through the
//     model → tool → stop loop. Both root and subagent sessions use
//     the same engine; the only variation is the [wakePolicy] that
//     decides what happens after the model stops.
//
//   - [rootWakePolicy] pops follow-ups, waits for subagent envelopes,
//     or terminates. [childWakePolicy] publishes a turn envelope to the
//     parent, then waits for parent messages or descendant updates.
//
//   - [EventBus] fans out per-session events to multiple observers
//     (TUI tabs, recorders, attached sessions). Non-blocking on the
//     publish path; slow subscribers drop events.
//
//   - [SessionRecorder] persists session changes asynchronously via
//     per-session worker goroutines registered as a global EventBus
//     observer.
//
// # Subagent sessions
//
// Subagent sessions run through the same [sessionRunner.runStreamWithConfig]
// entry point as root sessions. [LocalRuntime.StartChildLoop] constructs a
// per-child [sessionState] and [sessionRunner] that shares the root
// runtime's [runtimeCore], then drives the child through the unified
// engine with a [childWakePolicy].
//
// The subagent lifecycle (start, send, inspect, finalize, stop) is
// managed by [pkg/subagent.Manager]. Tool handlers for the subagent_*
// tools live in subagent_tools.go; the child-loop runner implementation
// lives in subagent_runner.go; envelope injection and parent-idle wait
// logic live in session_runner.go and subagent_envelopes.go.
package runtime
