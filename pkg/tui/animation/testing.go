package animation

import (
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TickCmdForTest returns an immediate current-generation tick command whose
// accepted clock delta is deterministic. It is intended for event-loop tests
// that must exercise Runtime.Accept without sleeping.
func TickCmdForTest(runtime *Runtime, delta time.Duration) tea.Cmd {
	runtime.mustExist()
	runtime.mu.Lock()
	if delta <= 0 {
		delta = TickRate
	}
	runtime.tickScheduled = true
	runtimeIdentity, generation := runtime.runtimeIdentity, runtime.generation
	deliveredAt := runtime.lastDeliveredAt.Add(delta)
	if runtime.lastDeliveredAt.IsZero() {
		deliveredAt = time.Unix(1, 0).Add(delta)
	}
	timerStartedAt := deliveredAt.Add(-delta)
	runtime.mu.Unlock()
	return func() tea.Msg {
		return TickMsg{runtimeIdentity: runtimeIdentity, generation: generation, deliveredAt: deliveredAt, timerStartedAt: timerStartedAt}
	}
}

// TickMsgForTest creates a current-generation tick and advances the animation runtime's
// elapsed clock for component tests that fan out an already-accepted tick.
func TickMsgForTest(runtime *Runtime, elapsed time.Duration) TickMsg {
	runtime.mustExist()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	elapsedBeforeTick := runtime.elapsed
	runtime.elapsed = elapsed
	msg := TickMsg{runtimeIdentity: runtime.runtimeIdentity, generation: runtime.generation, dirty: &atomic.Bool{}, elapsedBeforeTick: elapsedBeforeTick, elapsedAfterTick: elapsed}
	return msg
}
