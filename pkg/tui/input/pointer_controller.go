package input

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

const pointerFlushInterval = 16 * time.Millisecond

// PointerController applies one global cadence to hover motion and wheel input.
// Button interactions bypass the cadence so drags and their boundaries remain
// lossless.
type PointerController struct {
	mu sync.Mutex

	motion     tea.MouseMotionMsg
	hasMotion  bool
	wheel      int
	hasWheel   bool
	x, y       int
	scheduled  bool
	pressed    bool
	generation uint64

	send func(tea.Msg)
}

// NewPointerController creates a pointer input controller.
func NewPointerController() *PointerController { return &PointerController{} }

// SetSender wires the message sender used to emit cadence updates.
func (c *PointerController) SetSender(send func(tea.Msg)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.send = send
}

// Filter consumes coalescible pointer messages and returns the message that
// should enter Bubble Tea's update loop. Clicks, releases, and drag motion are
// always returned immediately.
func (c *PointerController) Filter(msg tea.Msg) tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		delta, ok := wheelDelta(msg)
		if !ok {
			return msg
		}
		c.wheel += delta
		c.hasWheel = true
		c.x, c.y = msg.X, msg.Y
		// Wheel coordinates are the current pointer position, so an older
		// standalone motion need not be emitted separately.
		c.hasMotion = false
		c.scheduleLocked()
		return nil

	case tea.MouseMotionMsg:
		if c.pressed || msg.Button == tea.MouseLeft {
			return msg
		}
		if c.hasWheel {
			// A wheel report already carries pointer coordinates for this
			// interval. Keep those authoritative and suppress duplicate motion.
			return nil
		}
		c.motion = msg
		c.hasMotion = true
		c.x, c.y = msg.X, msg.Y
		c.scheduleLocked()
		return nil

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			c.pressed = true
		}
		return c.boundaryLocked(msg)

	case tea.MouseReleaseMsg:
		result := c.boundaryLocked(msg)
		if msg.Button == tea.MouseLeft {
			c.pressed = false
		}
		return result
	}
	return msg
}

func (c *PointerController) scheduleLocked() {
	if c.scheduled {
		return
	}
	c.scheduled = true
	c.generation++
	generation := c.generation
	time.AfterFunc(pointerFlushInterval, func() { c.flush(generation) })
}

func (c *PointerController) boundaryLocked(event tea.Msg) tea.Msg {
	pending, ok := c.takeLocked()
	if !ok {
		return event
	}
	// Invalidate the timer whose pending update was taken at this boundary.
	c.generation++
	return messages.PointerBoundaryMsg{Pending: pending, Event: event}
}

func (c *PointerController) flush(generation uint64) {
	c.mu.Lock()
	if generation != c.generation {
		c.mu.Unlock()
		return
	}
	pending, ok := c.takeLocked()
	send := c.send
	c.mu.Unlock()
	if ok && send != nil {
		send(pending)
	}
}

func (c *PointerController) takeLocked() (messages.PointerUpdateMsg, bool) {
	if !c.scheduled && !c.hasMotion && c.wheel == 0 {
		return messages.PointerUpdateMsg{}, false
	}
	pending := messages.PointerUpdateMsg{X: c.x, Y: c.y, WheelDelta: c.wheel, Wheel: c.hasWheel}
	if c.hasMotion {
		motion := c.motion
		pending.Motion = &motion
	}
	c.hasMotion = false
	c.wheel = 0
	c.hasWheel = false
	c.scheduled = false
	return pending, pending.Motion != nil || pending.Wheel
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
