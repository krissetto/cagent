// Package inbox provides shared concurrency primitives for the runtime's
// per-session steer/follow-up queues and the subagent layer's parent inbox
// and direct child inbox.
//
// The two layers historically had near-identical implementations of
//
//   - a single-slot "signal" notification channel with stale-tick safety
//   - a FIFO message queue with drain semantics
//
// but lived in different packages and drifted in subtle ways. This package
// is the single source of truth for those primitives, importable from both
// pkg/runtime and pkg/subagent without introducing a dependency cycle.
package inbox

// Signal is a single-slot buffered notification primitive used by both
// the runtime's per-session message queues (steer/follow-up) and the
// subagent layer's parent envelope and direct child inboxes.
//
// Coordination pattern shared by every caller:
//
//   - producers call [Signal.Notify] to wake any waiter
//   - consumers call [Signal.Consume] in the same critical section that
//     observes the underlying data, so a buffered tick can never outlive
//     its corresponding payload
//
// We extracted this primitive after the "stale tick" class of bugs bit
// twice in the subagent layer (parent envelope inbox, then per-handle
// message queue): every call site had to remember to drop the buffered
// tick after draining its items, and the original implementations both
// forgot. Hiding the channel behind a small value type removes that
// possibility — the only operations callers can perform are Notify,
// Consume, and C.
//
// Signal is a small value type: it is cheap to embed, safe to copy
// (the channel is a reference type), and never reallocated after
// construction.
type Signal struct {
	ch chan struct{}
}

// NewSignal returns a fresh single-slot signal.
func NewSignal() Signal {
	return Signal{ch: make(chan struct{}, 1)}
}

// Notify wakes any waiter. Multiple Notify calls coalesce into a single
// buffered tick — consumers must always re-check the underlying data
// after receiving on [Signal.C].
func (s Signal) Notify() {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// Consume drops any buffered tick. Drains or observes of the underlying
// data must call Consume in the same critical section so a future select
// cannot fire on a stale tick after the data has already been drained.
func (s Signal) Consume() {
	select {
	case <-s.ch:
	default:
	}
}

// C returns the receive channel for use in select statements.
func (s Signal) C() <-chan struct{} { return s.ch }
