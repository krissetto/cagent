package messages

import tea "charm.land/bubbletea/v2"

// PointerUpdateMsg is one coherent cadence update. Wheel coordinates always
// identify the current pointer position; Motion is present only when a
// standalone hover update must also be routed.
type PointerUpdateMsg struct {
	X, Y       int
	WheelDelta int
	Wheel      bool
	Motion     *tea.MouseMotionMsg
}

// PointerBoundaryMsg keeps a pending cadence update ordered immediately before
// a lossless click or release in the root update loop.
type PointerBoundaryMsg struct {
	Pending PointerUpdateMsg
	Event   tea.Msg
}
