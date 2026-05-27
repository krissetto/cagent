# Stack 03: Runtime-managed subagents

## Implemented scope

Stack 03 adds runtime-managed subagent support on top of the durable public runtime events from Stack 02.

Implemented pieces:

- New runtime-managed subagent toolset registered through team loading when an agent declares `subagents`.
- Tool surface for starting, sending to, inspecting, listing, stopping, and finalizing runtime-managed child sessions.
- Managed child sessions are created as runtime-managed subsessions linked to the parent session tree via `AddSubSession`.
- Child runtime events are retained for inspect/list behavior and are persisted through the public runtime event observer.
- Durable `subagent_lifecycle` events are emitted for running/completed/failed/stopped lifecycle states.
- Child runtime error events mark the managed subagent as `failed`; the error is exposed through `subagent_list`, `subagent_inspect`, and lifecycle replay payloads.
- `agent-schema.json`, docs, and `examples/runtime_subagents.yaml` document the `subagents` config surface.

## Validation

Targeted tests cover:

- starting runtime-managed child sessions and replaying lifecycle/public runtime events;
- child session topology and root preservation;
- failed child runtimes and replayed failed lifecycle payloads;
- cross-root access rejection;
- queue isolation for follow-up/steer messages;
- finalize and stop behavior;
- whitespace-only `subagent_send` rejection;
- schema validation for examples/docs via existing config schema tests.

## Known assumptions and gaps

- Runtime-managed subagents are intended for trusted agents already listed in the caller's `subagents` allow-list.
- `subagent_stop` marks live status as stopped immediately and cancels the child; final stream drain avoids downgrading a later child failure to stopped.
- Parent session topology stores a subsession reference; child transcript content remains in the child session and is surfaced through inspect/replay, not duplicated as parent chat messages.
- The implementation uses existing runtime queues and persistence observers rather than a separate scheduler service.
