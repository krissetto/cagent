//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// OpenDialogMsg is sent to open a new dialog.
//
// OriginatingEvent is an optional runtime event whose presence marks the
// dialog as a background dialog. Background dialogs do not block tab
// navigation: tab-switch keys and tab-bar mouse clicks keep working. When
// the user switches away from the tab that opened the dialog, the dialog is
// closed and OriginatingEvent is re-stashed in the supervisor so the same
// prompt is re-displayed when the user returns. Other input (including
// mouse-wheel events) is still routed to the dialog while it is on screen.
type OpenDialogMsg struct {
	Model            Dialog
	OriginatingEvent tea.Msg
}

// CloseDialogMsg is sent to close the current (topmost) dialog
type CloseDialogMsg struct{}

// CloseAllDialogsMsg is sent to close all dialogs in the stack
type CloseAllDialogsMsg struct{}

// Dialog defines the interface that all dialogs must implement
type Dialog interface {
	layout.Model
	Position() (int, int) // Returns (row, col) for dialog placement
}

// OutsideClickDismisser allows mandatory dialogs to explicitly ignore the
// default outside-left-click cancellation policy. Returning nil keeps the
// dialog open. Outside clicks must never perform an affirmative action.
type OutsideClickDismisser interface {
	OutsideClickDismissCmd() tea.Cmd
}

// SemanticCloser defines the cancellation transaction for shared close chrome
// and outside-click dismissal. It must be identical to the dialog's Escape
// path, including protocol responses and preview rollback.
type SemanticCloser interface {
	CancelDialogCmd() tea.Cmd
}

// ClosePolicy explicitly disables shared close chrome for decisions that must
// be answered in-dialog. Dialogs are closable by default.
type ClosePolicy interface {
	DialogClosable() bool
}

// Manager manages the dialog stack and rendering
type Manager interface {
	layout.Model

	GetLayers() []*lipgloss.Layer
	Open() bool
	TopIsExitConfirmation() bool
	// TopIsBackground reports whether the topmost dialog is a background
	// dialog (i.e. it should not block tab navigation).
	TopIsBackground() bool
	// TopBackgroundEvent returns the originating event of the topmost
	// background dialog, or nil if the top dialog is not a background dialog
	// or the dialog stack is empty.
	TopBackgroundEvent() tea.Msg
	// TopDialog returns the topmost dialog instance, or nil if the dialog
	// stack is empty. Used by the app model to stash a background dialog's
	// live state when the user navigates away from the tab that opened it,
	// so the same instance (with any in-progress input) can be re-opened on
	// return.
	TopDialog() Dialog
	// TakeVisualDirty reports and clears an explicit pointer-driven visible mutation.
	TakeVisualDirty() bool
}

// dialogEntry pairs a dialog with its drag offset so the two stay in sync.
type dialogEntry struct {
	dialog       Dialog
	closeHovered bool // hover belongs to this stack entry's open lifecycle
	offsetX      int  // accumulated horizontal drag displacement
	offsetY      int  // accumulated vertical drag displacement
	// originatingEvent is the runtime event that caused this dialog to open,
	// when applicable. A non-nil value marks the dialog as a background
	// dialog (see OpenDialogMsg.OriginatingEvent).
	originatingEvent tea.Msg
}

// dragState tracks an in-progress drag operation.
type dragState struct {
	active bool
	startX int // screen X where drag began
	startY int // screen Y where drag began
	origDX int // dialog offsetX at drag start
	origDY int // dialog offsetY at drag start
}

// manager implements Manager
type manager struct {
	width, height int
	stack         []dialogEntry
	drag          dragState
	visualDirty   bool
}

// New creates a new dialog component manager
func New() Manager {
	return &manager{}
}

// Init initializes the dialog component
func (d *manager) Init() tea.Cmd {
	return nil
}

// Update handles messages and updates dialog state
func (d *manager) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.width = msg.Width
		d.height = msg.Height
		cmd := d.broadcastToAll(msg)
		return d, cmd

	case messages.ThemeChangedMsg:
		cmd := d.broadcastToAll(msg)
		return d, cmd

	case OpenDialogMsg:
		return d.handleOpen(msg)

	case CloseDialogMsg:
		return d.handleClose()

	case CloseAllDialogsMsg:
		return d.handleCloseAll()

	case tea.KeyPressMsg:
		// Ctrl-C is a cancellation gesture while a dialog is open. The root
		// decides whether to open exit confirmation only when no dialog exists;
		// there is no special case for an exit-confirmation dialog here.
		if key.Matches(msg, core.GetKeys().Quit) {
			return d, d.semanticCloseTop()
		}

	case tea.MouseClickMsg:
		adjusted := d.adjustMouseMsg(msg)
		if msg.Button == tea.MouseLeft {
			if cmd := d.handleOutsideClickDismiss(msg); cmd != nil {
				return d, cmd
			}
			// Only the narrow close control gets first refusal in the title
			// zone. All other title clicks start dragging before ordinary
			// dialog body hit testing (for example a broad URL click handler).
			if d.closeButtonHit(msg.X, msg.Y) {
				return d, d.semanticCloseTop()
			}
			if d.handleDragStart(msg.X, msg.Y) {
				return d, nil
			}
			return d, d.forwardToTop(adjusted)
		}
		cmd := d.forwardToTop(adjusted)
		return d, cmd

	case tea.MouseMotionMsg:
		if len(d.stack) > 0 {
			top := len(d.stack) - 1
			hovered := d.closeButtonHit(msg.X, msg.Y)
			d.visualDirty = d.visualDirty || hovered != d.stack[top].closeHovered
			d.stack[top].closeHovered = hovered
		}
		if d.drag.active {
			d.visualDirty = d.handleDragMotion(msg.X, msg.Y) || d.visualDirty
			return d, nil
		}
		cmd := d.forwardToTop(d.adjustMouseMsg(msg))
		d.takeTopVisualDirty()
		return d, cmd

	case tea.MouseReleaseMsg:
		if d.drag.active {
			d.drag.active = false
			return d, nil
		}
		cmd := d.forwardToTop(d.adjustMouseMsg(msg))
		return d, cmd

	case tea.MouseWheelMsg:
		cmd := d.forwardToTop(d.adjustMouseMsg(msg))
		d.takeTopVisualDirty()
		return d, cmd

	case messages.WheelCoalescedMsg:
		cmd := d.forwardToTop(d.adjustMouseMsg(msg))
		d.takeTopVisualDirty()
		return d, cmd
	}

	// Forward non-mouse messages to top dialog
	cmd := d.forwardToTop(msg)
	return d, cmd
}

// View renders all dialogs (used for debugging, actual rendering uses GetLayers)
func (d *manager) View() string {
	if len(d.stack) == 0 {
		return ""
	}
	return d.entryView(&d.stack[len(d.stack)-1])
}

// broadcastToAll sends a message to every dialog in the stack and batches the resulting commands.
func (d *manager) broadcastToAll(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for i := range d.stack {
		u, cmd := d.stack[i].dialog.Update(msg)
		d.stack[i].dialog = u.(Dialog)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// forwardToTop forwards a message to the topmost dialog and returns the resulting command.
func (d *manager) forwardToTop(msg tea.Msg) tea.Cmd {
	if len(d.stack) == 0 {
		return nil
	}
	top := len(d.stack) - 1
	u, cmd := d.stack[top].dialog.Update(msg)
	d.stack[top].dialog = u.(Dialog)
	return cmd
}

// titleZoneHeight is the number of rows from the top of a dialog that form
// the draggable title zone: border top + padding top + title line + separator.
const titleZoneHeight = 4

// handleDragStart checks if a mouse click is in the title zone of the topmost
// dialog (border, padding, title text, and separator). If so, it initiates a
// drag operation and returns true.
func (d *manager) handleDragStart(x, y int) bool {
	if len(d.stack) == 0 {
		return false
	}
	top := len(d.stack) - 1
	e := &d.stack[top]

	row, col := e.dialog.Position()
	row += e.offsetY
	col += e.offsetX
	w := lipgloss.Width(e.dialog.View())

	// Check horizontal bounds
	if x < col || x >= col+w {
		return false
	}
	// Check vertical bounds: click must be within the title zone
	if y < row || y >= row+titleZoneHeight {
		return false
	}

	d.drag = dragState{
		active: true,
		startX: x,
		startY: y,
		origDX: e.offsetX,
		origDY: e.offsetY,
	}
	return true
}

// handleDragMotion updates the drag offset during a drag operation.
func (d *manager) handleDragMotion(x, y int) bool {
	if len(d.stack) == 0 {
		return false
	}
	e := &d.stack[len(d.stack)-1]
	oldX, oldY := e.offsetX, e.offsetY
	row, col := e.dialog.Position()
	view := e.dialog.View()
	w, h := lipgloss.Width(view), lipgloss.Height(view)
	proposedX := d.drag.origDX + (x - d.drag.startX)
	proposedY := d.drag.origDY + (y - d.drag.startY)
	// Keep the complete dialog in the terminal viewport when it fits; for an
	// oversized dialog, keep its top-left reachable.
	maxCol := max(0, d.width-w)
	maxRow := max(0, d.height-h)
	e.offsetX = min(max(proposedX, -col), maxCol-col)
	e.offsetY = min(max(proposedY, -row), maxRow-row)
	return oldX != e.offsetX || oldY != e.offsetY
}

// adjustMouseMsg adjusts mouse coordinates in a message to account for the drag offset
// of the top dialog, so that the dialog's internal hit-testing works correctly.
func (d *manager) adjustMouseMsg(msg tea.Msg) tea.Msg {
	if len(d.stack) == 0 {
		return msg
	}
	e := d.stack[len(d.stack)-1]
	if e.offsetX == 0 && e.offsetY == 0 {
		return msg
	}

	switch m := msg.(type) {
	case tea.MouseClickMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case tea.MouseMotionMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case tea.MouseReleaseMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case messages.WheelCoalescedMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case tea.MouseWheelMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	}
	return msg
}

// closeButtonHit reports whether screen coordinates hit the one-cell close
// control on the top border of the topmost dialog.
func (d *manager) closeButtonHit(x, y int) bool {
	if len(d.stack) == 0 || !dialogClosable(d.stack[len(d.stack)-1].dialog) {
		return false
	}
	e := d.stack[len(d.stack)-1]
	row, col := e.dialog.Position()
	row += e.offsetY
	col += e.offsetX
	width := lipgloss.Width(e.dialog.View())
	return y == row+styles.DialogStyle.GetBorderTopSize() &&
		x == col+width-styles.DialogStyle.GetBorderRightSize()-1-dialogCloseInset
}

func dialogClosable(dialog Dialog) bool {
	if policy, ok := dialog.(ClosePolicy); ok && !policy.DialogClosable() {
		return false
	}
	_, ok := dialog.(SemanticCloser)
	return ok
}

func (d *manager) semanticCloseTop() tea.Cmd {
	if len(d.stack) == 0 {
		return nil
	}
	closer, ok := d.stack[len(d.stack)-1].dialog.(SemanticCloser)
	if !ok {
		return nil
	}
	return closer.CancelDialogCmd()
}

func (d *manager) entryView(e *dialogEntry) string {
	view := e.dialog.View()
	if dialogClosable(e.dialog) {
		view = renderCloseControl(view, e.closeHovered)
	}
	return clampRenderedFrame(view, d.width, d.height)
}

func clampRenderedFrame(view string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "")
	}
	return strings.Join(lines, "\n")
}

// handleOutsideClickDismiss applies cancellation on an outside left click
// while preserving each entry's drag offset. Mandatory dialogs may opt out;
// no outside click is allowed to select an affirmative action.
func (d *manager) handleOutsideClickDismiss(msg tea.MouseClickMsg) tea.Cmd {
	if len(d.stack) == 0 {
		return nil
	}
	e := d.stack[len(d.stack)-1]
	row, col := e.dialog.Position()
	row += e.offsetY
	col += e.offsetX
	view := e.dialog.View()
	if msg.X >= col && msg.X < col+lipgloss.Width(view) && msg.Y >= row && msg.Y < row+lipgloss.Height(view) {
		return nil
	}
	if dismisser, ok := e.dialog.(OutsideClickDismisser); ok {
		return dismisser.OutsideClickDismissCmd()
	}
	return d.semanticCloseTop()
}

func resetDialogCloseHover(dialog Dialog) {
	if resetter, ok := dialog.(interface{ ResetCloseHover() }); ok {
		resetter.ResetCloseHover()
	}
}

// handleOpen processes dialog opening requests and adds to stack
func (d *manager) handleOpen(msg OpenDialogMsg) (layout.Model, tea.Cmd) {
	resetDialogCloseHover(msg.Model)
	d.stack = append(d.stack, dialogEntry{
		dialog:           msg.Model,
		originatingEvent: msg.OriginatingEvent,
	})

	var cmds []tea.Cmd
	cmd := msg.Model.Init()
	cmds = append(cmds, cmd)

	_, cmd = msg.Model.Update(tea.WindowSizeMsg{
		Width:  d.width,
		Height: d.height,
	})
	cmds = append(cmds, cmd)

	return d, tea.Batch(cmds...)
}

// handleClose processes dialog closing requests (pops top dialog from stack)
func (d *manager) handleClose() (layout.Model, tea.Cmd) {
	if len(d.stack) > 0 {
		d.stack = d.stack[:len(d.stack)-1]
	}
	d.drag.active = false
	if len(d.stack) > 0 {
		top := len(d.stack) - 1
		d.stack[top].closeHovered = false
		resetDialogCloseHover(d.stack[top].dialog)
	}
	return d, nil
}

// handleCloseAll closes all dialogs in the stack
func (d *manager) handleCloseAll() (layout.Model, tea.Cmd) {
	d.stack = nil
	d.drag.active = false
	return d, nil
}

// Open returns true if there is at least one active dialog
func (d *manager) Open() bool {
	return len(d.stack) > 0
}

// TopIsExitConfirmation returns true if the topmost dialog is the exit
// confirmation dialog. Used by the top-level key handler to route ctrl+c to
// the exit confirmation (which exits the program) instead of stacking another
// exit confirmation on top.
func (d *manager) TopIsExitConfirmation() bool {
	if len(d.stack) == 0 {
		return false
	}
	_, ok := d.stack[len(d.stack)-1].dialog.(*exitConfirmationDialog)
	return ok
}

// TopIsBackground returns true if the topmost dialog is a background dialog
// (opened with a non-nil OriginatingEvent). See OpenDialogMsg for semantics.
func (d *manager) TopIsBackground() bool {
	if len(d.stack) == 0 {
		return false
	}
	return d.stack[len(d.stack)-1].originatingEvent != nil
}

// TopBackgroundEvent returns the originating event of the topmost background
// dialog, or nil if the top dialog is not a background dialog or the dialog
// stack is empty.
func (d *manager) TopBackgroundEvent() tea.Msg {
	if len(d.stack) == 0 {
		return nil
	}
	return d.stack[len(d.stack)-1].originatingEvent
}

// TopDialog returns the topmost dialog instance, or nil if the dialog stack
// is empty.
func (d *manager) TopDialog() Dialog {
	if len(d.stack) == 0 {
		return nil
	}
	return d.stack[len(d.stack)-1].dialog
}

func (d *manager) takeTopVisualDirty() {
	if len(d.stack) == 0 {
		return
	}
	if dirty, ok := d.stack[len(d.stack)-1].dialog.(interface{ TakeVisualDirty() bool }); ok {
		d.visualDirty = dirty.TakeVisualDirty() || d.visualDirty
	}
}

func (d *manager) TakeVisualDirty() bool {
	dirty := d.visualDirty
	d.visualDirty = false
	return dirty
}

func (d *manager) SetSize(width, height int) tea.Cmd {
	d.width = width
	d.height = height
	return nil
}

// CenterPosition calculates the centered position for a dialog given screen and dialog dimensions.
// Returns (row, col) suitable for use in Dialog.Position().
func CenterPosition(screenWidth, screenHeight, dialogWidth, dialogHeight int) (row, col int) {
	col = max(0, (screenWidth-dialogWidth)/2)
	row = max(0, (screenHeight-dialogHeight)/2)

	// Ensure dialog fits on screen
	col = min(col, max(0, screenWidth-dialogWidth))
	row = min(row, max(0, screenHeight-dialogHeight))

	return row, col
}

// GetLayers returns lipgloss layers for rendering all dialogs in the stack
// Dialogs are returned in order from bottom to top (index 0 is bottom-most)
func (d *manager) GetLayers() []*lipgloss.Layer {
	if len(d.stack) == 0 {
		return nil
	}

	layers := make([]*lipgloss.Layer, 0, len(d.stack))
	for _, e := range d.stack {
		view := d.entryView(&e)
		row, col := e.dialog.Position()
		layers = append(layers, lipgloss.NewLayer(view).X(col+e.offsetX).Y(row+e.offsetY))
	}

	return layers
}
