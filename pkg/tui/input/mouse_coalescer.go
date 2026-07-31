package input

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

const mouseFlushInterval = 16 * time.Millisecond

// MouseCoalescer batches motion and wheel input on a 16 ms cadence.
// Clicks and releases flush pending input first.
type MouseCoalescer struct {
	mu sync.Mutex

	latestMotion  tea.MouseMotionMsg
	hasMotion     bool
	wheelDelta    int
	hasWheel      bool
	pointerX      int
	pointerY      int
	timerPending  bool
	timerSequence uint64

	sender func(tea.Msg)
}

// NewMouseCoalescer creates a mouse input coalescer.
func NewMouseCoalescer() *MouseCoalescer { return &MouseCoalescer{} }

// SetSender sets the function used to send coalesced updates.
func (c *MouseCoalescer) SetSender(sender func(tea.Msg)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sender = sender
}

// Filter coalesces motion and wheel messages. It returns clicks and releases
// immediately, with pending input ordered before them.
func (c *MouseCoalescer) Filter(msg tea.Msg) tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		delta, ok := wheelDelta(msg)
		if !ok {
			return msg
		}
		c.wheelDelta += delta
		c.hasWheel = true
		c.pointerX, c.pointerY = msg.X, msg.Y
		// Wheel input supplies the latest pointer position.
		c.hasMotion = false
		c.scheduleLocked()
		return nil

	case tea.MouseMotionMsg:
		if c.hasWheel {
			// Wheel position takes precedence within the current interval.
			return nil
		}
		c.latestMotion = msg
		c.hasMotion = true
		c.pointerX, c.pointerY = msg.X, msg.Y
		c.scheduleLocked()
		return nil

	case tea.MouseClickMsg:
		return c.boundaryLocked(msg)

	case tea.MouseReleaseMsg:
		return c.boundaryLocked(msg)
	}
	return msg
}

func (c *MouseCoalescer) scheduleLocked() {
	if c.timerPending {
		return
	}
	c.timerPending = true
	c.timerSequence++
	sequence := c.timerSequence
	time.AfterFunc(mouseFlushInterval, func() { c.flush(sequence) })
}

func (c *MouseCoalescer) boundaryLocked(event tea.Msg) tea.Msg {
	update, ok := c.takeLocked()
	if !ok {
		return event
	}
	c.timerSequence++
	return messages.PointerBoundaryMsg{Pending: update, Event: event}
}

func (c *MouseCoalescer) flush(sequence uint64) {
	c.mu.Lock()
	if sequence != c.timerSequence {
		c.mu.Unlock()
		return
	}
	update, ok := c.takeLocked()
	sender := c.sender
	c.mu.Unlock()
	if ok && sender != nil {
		sender(update)
	}
}

func (c *MouseCoalescer) takeLocked() (messages.PointerUpdateMsg, bool) {
	if !c.timerPending && !c.hasMotion && c.wheelDelta == 0 {
		return messages.PointerUpdateMsg{}, false
	}
	update := messages.PointerUpdateMsg{
		X:          c.pointerX,
		Y:          c.pointerY,
		WheelDelta: c.wheelDelta,
		HasWheel:   c.hasWheel,
	}
	if c.hasMotion {
		motion := c.latestMotion
		update.Motion = &motion
	}
	c.hasMotion = false
	c.wheelDelta = 0
	c.hasWheel = false
	c.timerPending = false
	return update, update.Motion != nil || update.HasWheel
}

func wheelDelta(msg tea.MouseWheelMsg) (int, bool) {
	switch msg.Button {
	case tea.MouseWheelUp:
		return -1, true
	case tea.MouseWheelDown:
		return 1, true
	default:
		return 0, false
	}
}
