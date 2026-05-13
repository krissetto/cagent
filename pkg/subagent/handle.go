package subagent

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/inbox"
	"github.com/docker/docker-agent/pkg/session"
)

// Handle is the live representation of a subagent. It exposes the
// handful of hooks the runtime's child loop actually uses (close/interrupt
// signals, inbox draining, publish helpers) plus read-only accessors for the
// manager and tool layer.
//
// A Handle belongs to exactly one parent. It is created by
// [Manager.StartChild] and must not be constructed directly.
type Handle struct {
	id              string
	parentSessionID string
	agentName       string
	title           atomicString
	intent          atomicString
	externalID      atomicString
	depth           int
	createdAt       time.Time
	maxPreview      int

	// childSession is the session that the loop operates on.
	childSession *session.Session

	// status holds the lifecycle state (atomic so fast-path reads avoid
	// lock contention). Stored as int32 for direct use of [atomic.Int32];
	// callers should always go through Status(h.status.Load()) so the
	// integer encoding stays an internal detail.
	status atomic.Int32

	// stopFn cancels the child loop's outer context when the caller invokes
	// Stop. It is set atomically at construction so concurrent Stop calls
	// observe a consistent value without racing with handle initialization.
	stopFn atomic.Pointer[func()]

	// loopDone is closed exactly once when the child loop has finished.
	// It is pre-allocated in newHandle so [Manager.StopAll] always has a
	// non-nil channel to wait on, even if it observes the handle in m.byID
	// before the manager's forwarder goroutine wires the Runner's done
	// channel into it. The manager owns closing this channel via a small
	// forwarder goroutine that bridges the [Runner.StartChildLoop] result.
	loopDone chan struct{}

	// turnStopMu guards turnStopFn. The child loop and user-initiated
	// interrupt requests race on the same field.
	turnStopMu sync.Mutex
	turnStopFn context.CancelFunc

	// inbox carries follow-up messages from parent → child loop.
	inbox *inbox.Queue[Message]

	// steerInbox carries steer-mode messages from parent → child loop.
	steerInbox *inbox.Queue[Message]

	// closeCh is closed when the parent asks the loop to terminate
	// cleanly.
	closeCh   chan struct{}
	closeOnce sync.Once

	// lastPreview / lastErr / lastUpdateAt track the most recent
	// envelope payload for snapshots and envelope synthesis.
	lastPreview  atomicString
	lastErr      atomicString
	lastUpdateAt atomicTime

	// parked is set atomically when MarkWaitingSilently is called (interrupted
	// mid-turn) and cleared by MarkRunning. It signals to HasInFlightChildren
	// that this waiting child may still produce future output without receiving
	// new parent-inbox work, so the parent loop must not exit while it is set.
	parked atomic.Bool

	// publish hands envelopes to the manager. The manager injects a
	// closure that knows how to route them to the parent's mailbox.
	publish func(Envelope)
}

// newHandle constructs a fresh Handle. It is package-private so the
// manager is the sole source of Handles.
func newHandle(cfg StartConfig, childSession *session.Session, depth int, stopFn func(), publish func(Envelope)) *Handle {
	h := &Handle{
		id:              childSession.ID,
		parentSessionID: cfg.Parent.ID,
		agentName:       cfg.AgentName,
		depth:           depth,
		createdAt:       time.Now(),
		maxPreview:      cfg.MaxPreview,
		childSession:    childSession,
		inbox:           inbox.NewUnboundedQueue[Message](),
		steerInbox:      inbox.NewUnboundedQueue[Message](),

		closeCh:  make(chan struct{}),
		loopDone: make(chan struct{}),
		publish:  publish,
	}
	h.stopFn.Store(&stopFn)
	h.title.Store(cfg.Title)
	h.intent.Store(cfg.Intent)
	if h.maxPreview <= 0 {
		h.maxPreview = DefaultPreviewLimit
	}
	h.status.Store(int32(StatusStarting))
	return h
}

// ID returns the stable subagent identifier. It is also the child
// session's ID.
func (h *Handle) ID() string { return h.id }

// ParentSessionID returns the identifier of the parent session that
// owns this subagent.
func (h *Handle) ParentSessionID() string { return h.parentSessionID }

// AgentName returns the configured agent name driving the subagent.
func (h *Handle) AgentName() string { return h.agentName }

// Depth returns how many levels below a root session this subagent
// sits. Depth 1 means this subagent is a direct child of a root
// session.
func (h *Handle) Depth() int { return h.depth }

// Title returns the human-readable label of the subagent session.
func (h *Handle) Title() string { return h.title.Load() }

// Intent returns the optional semantic label describing why this subagent
// was spawned.
func (h *Handle) Intent() string { return h.intent.Load() }

// ExternalID returns the optional identifier of the corresponding child in
// an external execution system (for example an S2S platform session ID).
func (h *Handle) ExternalID() string { return h.externalID.Load() }

// CreatedAt returns the time the handle was registered with the
// manager.
func (h *Handle) CreatedAt() time.Time { return h.createdAt }

// Session returns the underlying child session. The runtime's child
// loop uses this to drive the loop; external callers should prefer the
// snapshot helpers.
func (h *Handle) Session() *session.Session { return h.childSession }

// Status returns the current lifecycle state.
func (h *Handle) Status() Status { return Status(h.status.Load()) }

// IsLive reports whether the subagent is still in a non-terminal state.
func (h *Handle) IsLive() bool { return !Status(h.status.Load()).IsTerminal() }

// LastPreview returns the most recent preview string emitted by the
// subagent, if any.
func (h *Handle) LastPreview() string { return h.lastPreview.Load() }

// Snapshot produces a read-only view of the subagent's state.
func (h *Handle) Snapshot() HandleSnapshot {
	return HandleSnapshot{
		ID:              h.id,
		ParentSessionID: h.parentSessionID,
		AgentName:       h.agentName,
		Title:           h.title.Load(),
		Intent:          h.intent.Load(),
		ExternalID:      h.externalID.Load(),
		Status:          Status(h.status.Load()),
		Depth:           h.depth,
		CreatedAt:       h.createdAt,
		LastUpdateAt:    h.lastUpdateAt.Load(),
		LastPreview:     h.lastPreview.Load(),
		Error:           h.lastErr.Load(),
	}
}

// --- Loop-facing API -------------------------------------------------
//
// The methods in this section are the contract between the child loop
// and the subagent core. They are named to read naturally from the
// loop's perspective.

// DrainInbox returns every follow-up parent-to-child message pending in
// the inbox. Safe to call from the child loop at its safe points.
func (h *Handle) DrainInbox() []Message { return h.inbox.Drain() }

// DrainSteerInbox returns every steer-mode parent-to-child message pending
// in the steer inbox. Safe to call from the child loop at its safe points.
func (h *Handle) DrainSteerInbox() []Message { return h.steerInbox.Drain() }

// InboxSignal returns the notification channel used by the follow-up
// parent→child inbox. The child loop selects on this alongside other
// wake-up sources and then calls DrainInbox to collect the actual
// messages.
func (h *Handle) InboxSignal() <-chan struct{} { return h.inbox.Signal() }

// SteerInboxSignal returns the notification channel used by the steer
// parent→child inbox. The child loop selects on this alongside other
// wake-up sources and then calls DrainSteerInbox to collect the actual
// messages.
func (h *Handle) SteerInboxSignal() <-chan struct{} { return h.steerInbox.Signal() }

// CloseCh returns a channel that is closed when the parent explicitly
// asks the loop to terminate. The loop should treat it as a request for
// a graceful exit at the next safe point.
func (h *Handle) CloseCh() <-chan struct{} { return h.closeCh }

// MarkRunning transitions the subagent to [StatusRunning]. Idempotent:
// calling it while already running is safe.
func (h *Handle) MarkRunning() {
	h.parked.Store(false)
	h.transitionNonTerminal(StatusRunning)
}

// MarkWaitingSilently transitions the subagent back to [StatusWaiting]
// without publishing an envelope. This is used for user interrupts: the
// current turn stops, but the parent should not be woken until the child
// actually completes a subsequent turn.
//
// It does, however, publish a [UpdateKindStatusOnly] envelope so the parent
// session's EventBus can refresh its sidebar row immediately without adding
// any transcript noise or waking the parent agent loop.
func (h *Handle) MarkWaitingSilently() {
	h.lastErr.Store("")
	now := time.Now()
	h.lastUpdateAt.Store(now)
	h.transitionNonTerminal(StatusWaiting)
	h.parked.Store(true)

	// Notify the parent's event bus of the status change so the sidebar
	// row flips from "running" to "waiting" immediately. The status-only
	// kind is intentionally excluded from the parent inbox (no envelope is
	// written there), so the parent agent loop does NOT wake as it would for
	// a real turn-completed update.
	h.publish(Envelope{
		SubAgentID:      h.id,
		ParentSessionID: h.parentSessionID,
		AgentName:       h.agentName,
		Kind:            UpdateKindStatusOnly,
		Status:          StatusWaiting,
		Preview:         h.lastPreview.Load(),
		At:              now,
	})
}

// PublishTurn emits a turn-completed envelope for the parent. The loop
// is expected to call this after each full turn while keep-alive.
func (h *Handle) PublishTurn(assistantMessage string) {
	preview, truncated := TruncatePreview(assistantMessage, h.maxPreview)
	h.lastPreview.Store(preview)
	h.lastErr.Store("")
	now := time.Now()
	h.lastUpdateAt.Store(now)
	h.transitionNonTerminal(StatusWaiting)
	h.parked.Store(false)

	h.publish(Envelope{
		SubAgentID:      h.id,
		ParentSessionID: h.parentSessionID,
		AgentName:       h.agentName,
		Kind:            UpdateKindTurnCompleted,
		Status:          StatusWaiting,
		Preview:         preview,
		Truncated:       truncated,
		At:              now,
	})
}

// PublishClosed emits a closed envelope. Must be the final emission
// from the loop when exiting due to a parent close request.
func (h *Handle) PublishClosed() {
	h.publishTerminal(StatusClosed, UpdateKindClosed, "")
}

// PublishStopped emits a stopped envelope. Used when the loop exits
// because the context was cancelled or the handle was stopped.
func (h *Handle) PublishStopped() {
	h.publishTerminal(StatusStopped, UpdateKindStopped, "")
}

// PublishFailure emits a failure envelope. Used when the loop exits
// because of a runtime error.
func (h *Handle) PublishFailure(err string) {
	h.publishTerminal(StatusFailed, UpdateKindFailed, err)
}

// HasPendingSteerInbox reports whether steer-mode parent→child messages
// are waiting to be consumed by the loop.
func (h *Handle) HasPendingSteerInbox() bool { return h.steerInbox.HasItems() }

// HasPendingInbox reports whether any parent→child messages are waiting
// to be consumed by the loop.
func (h *Handle) HasPendingInbox() bool {
	return h.inbox.HasItems() || h.steerInbox.HasItems()
}

// --- Manager-facing API ---------------------------------------------

// requestClose signals the child loop to exit cleanly. Safe to call
// multiple times.
func (h *Handle) requestClose() {
	h.closeOnce.Do(func() { close(h.closeCh) })
	h.inbox.Close()
	h.steerInbox.Close()
}

// cancel cancels the child loop's context. Safe to call multiple times.
func (h *Handle) cancel() {
	if fn := h.stopFn.Load(); fn != nil {
		(*fn)()
	}
}

// stopForcefully is the "hard stop" counterpart to requestClose. It asks the
// loop to shut down cooperatively at its next safe point and also cancels the
// outer loop context immediately so any in-flight provider/tool work unwinds.
func (h *Handle) stopForcefully() {
	h.requestClose()
	h.cancel()
}

// Interrupt aborts the currently-running child turn without cancelling the
// outer subagent lifetime. Safe to call even when no turn is in flight.
func (h *Handle) Interrupt() {
	h.turnStopMu.Lock()
	fn := h.turnStopFn
	h.turnStopMu.Unlock()
	if fn != nil {
		fn()
	}
}

// SetInterruptCancel wires the cancel function for the currently-running child
// turn. Passing nil clears any previous per-turn cancel.
func (h *Handle) SetInterruptCancel(fn context.CancelFunc) {
	h.turnStopMu.Lock()
	h.turnStopFn = fn
	h.turnStopMu.Unlock()
}

// SetTitle updates the handle's human-readable title. Safe for concurrent use.
func (h *Handle) SetTitle(title string) { h.title.Store(title) }

// SetIntent updates the handle's semantic spawn label. Safe for concurrent use.
func (h *Handle) SetIntent(intent string) { h.intent.Store(intent) }

// SetExternalID records the identifier of the corresponding child in an
// external execution system. Safe for concurrent use.
func (h *Handle) SetExternalID(id string) { h.externalID.Store(id) }

// LoopDone returns the child loop's completion channel. It is always non-nil
// from the moment [newHandle] returns, so callers (notably [Manager.StopAll])
// can rely on a select on this channel to observe loop termination without
// racing handle construction.
func (h *Handle) LoopDone() <-chan struct{} {
	return h.loopDone
}

// --- Internal helpers ------------------------------------------------

func (h *Handle) transitionNonTerminal(next Status) {
	for {
		cur := Status(h.status.Load())
		if cur.IsTerminal() {
			return
		}
		if cur == next {
			return
		}
		if h.status.CompareAndSwap(int32(cur), int32(next)) {
			return
		}
	}
}

func (h *Handle) publishTerminal(status Status, kind UpdateKind, errMsg string) {
	// First-to-terminal wins.
	for {
		cur := Status(h.status.Load())
		if cur.IsTerminal() {
			return
		}
		if h.status.CompareAndSwap(int32(cur), int32(status)) {
			break
		}
	}
	now := time.Now()
	h.lastUpdateAt.Store(now)
	if errMsg != "" {
		h.lastErr.Store(errMsg)
	}
	h.publish(Envelope{
		SubAgentID:      h.id,
		ParentSessionID: h.parentSessionID,
		AgentName:       h.agentName,
		Kind:            kind,
		Status:          status,
		Preview:         h.lastPreview.Load(),
		Error:           errMsg,
		At:              now,
	})
	h.inbox.Close()
	h.steerInbox.Close()
}
