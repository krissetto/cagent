package dialog

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Layout constants. These are deliberately tight so the dialog can grow or
// shrink gracefully on narrow / tall terminals without the numbers becoming
// magic sprinkled throughout the file.
const (
	sessionTreeWidthPercent  = 75
	sessionTreeMinWidth      = 70
	sessionTreeMaxWidth      = 120
	sessionTreeHeightPercent = 80
	sessionTreeMaxHeight     = 40

	// Fixed rows outside the scrollable body:
	//   title (1) + separator (1) + blank (1) = 3 header lines
	//   blank (1) + help line  (1)             = 2 footer lines
	sessionTreeHeaderLines = 3
	sessionTreeFooterLines = 2

	// sessionTreeChrome accounts for border + padding rendered by
	// [styles.DialogStyle]. Padding is 1 top + 1 bottom, border is 1 + 1.
	sessionTreeChrome = 4
)

// sessionTreeRow is the per-node mapping emitted by [buildSessionTree]: it
// lets the dialog translate a click-Y or a selected-index back into a node id
// and therefore into an [messages.OpenSubAgentTabMsg].
//
// A row may render on one or two lines (primary + optional detail); the line
// range lets hit testing treat both lines as part of the same selection.
type sessionTreeRow struct {
	Node      runtime.LiveSessionNode
	FirstLine int // index into the rendered body lines
	LastLine  int // inclusive
}

// sessionTreeDialog is a scrollable, selectable view of the live session tree.
//
// The dialog is interactive: up/down navigates, Enter opens the selected node,
// clicking a row opens it immediately. Opening a node dispatches an
// [messages.OpenSubAgentTabMsg], which the TUI routes either to an existing
// tab or to a newly-attached live tab. Falling back to
// [messages.OpenParentSessionMsg] would duplicate that flow, so we reuse the
// single entry point.
type sessionTreeDialog struct {
	BaseDialog

	nodes            []runtime.LiveSessionNode
	rootSessionID    string
	currentSessionID string
	renderedAt       time.Time

	scrollview *scrollview.Model
	keyMap     sessionTreeKeyMap

	// Selection state. rows is rebuilt every render because contentWidth may
	// change with window resize; selected is preserved across rebuilds by
	// matching on node session id.
	rows           []sessionTreeRow
	selected       int
	selectedNodeID string
}

// sessionTreeKeyMap groups the bindings the dialog uses directly. Anything
// not matched here falls through to the scrollview for page up/down, home/end.
type sessionTreeKeyMap struct {
	Up    key.Binding
	Down  key.Binding
	Enter key.Binding
	Close key.Binding
}

// NewSessionTreeDialog constructs a session-tree dialog.
//
// rootSessionID identifies the tree root. currentSessionID is the session id
// of the tab the dialog was opened from; the matching node starts selected so
// the user can press Enter to close + focus the current tab, or arrow away to
// another node without losing context.
func NewSessionTreeDialog(nodes []runtime.LiveSessionNode, rootSessionID, currentSessionID string) Dialog {
	return &sessionTreeDialog{
		nodes:            nodes,
		rootSessionID:    rootSessionID,
		currentSessionID: currentSessionID,
		renderedAt:       time.Now(),
		scrollview: scrollview.New(
			scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()),
			scrollview.WithReserveScrollbarSpace(true),
		),
		keyMap: sessionTreeKeyMap{
			Up:    key.NewBinding(key.WithKeys("up", "k")),
			Down:  key.NewBinding(key.WithKeys("down", "j")),
			Enter: key.NewBinding(key.WithKeys("enter")),
			Close: key.NewBinding(key.WithKeys("esc", "q")),
		},
		selectedNodeID: currentSessionID,
	}
}

// Init initializes the dialog. Nothing asynchronous to do.
func (d *sessionTreeDialog) Init() tea.Cmd { return nil }

// Bindings returns the dialog-specific bindings so the status bar can advertise
// them. Close/select keys are listed; scroll keys come from the scrollview.
func (d *sessionTreeDialog) Bindings() []key.Binding {
	return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Enter, d.keyMap.Close}
}

// Update handles messages. Selection/open events are intercepted here before
// delegating scroll-related events to the embedded scrollview.
func (d *sessionTreeDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return d, core.CmdHandler(CloseDialogMsg{})
		case key.Matches(msg, d.keyMap.Up):
			d.moveSelection(-1)
			return d, nil
		case key.Matches(msg, d.keyMap.Down):
			d.moveSelection(+1)
			return d, nil
		case key.Matches(msg, d.keyMap.Enter):
			return d.openSelected()
		}
	}

	// Delegate page up/down, home/end, wheel, drag, etc. to the scrollview
	// after consuming the dialog's own navigation keys.
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	if msg, ok := msg.(tea.MouseClickMsg); ok {
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		if idx := d.hitTestRow(msg.Y); idx >= 0 {
			d.selected = idx
			if idx < len(d.rows) {
				d.selectedNodeID = d.rows[idx].Node.SessionID
			}
			return d.openSelected()
		}
		return d, nil
	}

	return d, nil
}

// SetSize stores dialog dimensions and propagates to the scrollview.
func (d *sessionTreeDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)
	// SetSize may be called before View() has built rows, so we only push
	// scrollview dimensions here and let View() configure content.
	return nil
}

// Position centers the dialog.
func (d *sessionTreeDialog) Position() (row, col int) {
	dw, _ := d.dimensions()
	maxH := d.maxDialogHeight()
	return CenterPosition(d.Width(), d.Height(), dw, maxH)
}

// dimensions returns the outer dialog width and the inner content width,
// accounting for frame + scrollbar space reserved by the scrollview.
func (d *sessionTreeDialog) dimensions() (dialogWidth, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(sessionTreeWidthPercent, sessionTreeMinWidth, sessionTreeMaxWidth)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, contentWidth
}

// maxDialogHeight returns the cap we use for the dialog height. The actual
// rendered dialog may be shorter when the tree has few nodes.
func (d *sessionTreeDialog) maxDialogHeight() int {
	return min(d.Height()*sessionTreeHeightPercent/100, sessionTreeMaxHeight)
}

// View renders the dialog. Rebuilds rows based on current size and selection.
func (d *sessionTreeDialog) View() string {
	dialogWidth, contentWidth := d.dimensions()
	maxHeight := d.maxDialogHeight()

	// Rebuild rows so the layout reflects current width.
	body, rows := buildSessionTree(d.nodes, d.rootSessionID, d.currentSessionID, d.renderedAt, contentWidth)
	d.rows = rows

	// Restore selection by node id when possible (e.g. after a resize) so the
	// highlighted row stays on the same session.
	d.selected = d.resolveSelection()

	if len(body) == 0 {
		body = []string{styles.MutedStyle.Italic(true).Render("No live sessions to show yet.")}
	}

	// Apply the selection highlight to the primary line of the selected row.
	if sel := d.selectedRow(); sel != nil {
		for i := sel.FirstLine; i <= sel.LastLine && i < len(body); i++ {
			body[i] = applySelectionHighlight(body[i], contentWidth)
		}
	}

	// Size the scrollview: body height minus fixed chrome.
	visible := max(1, maxHeight-sessionTreeHeaderLines-sessionTreeFooterLines-sessionTreeChrome)
	visible = min(visible, len(body))
	visible = max(visible, 1)

	regionWidth := contentWidth + d.scrollview.ReservedCols()
	d.scrollview.SetSize(regionWidth, visible)
	d.scrollview.SetContent(body, len(body))

	// Keep the selected row in view when navigating with arrows.
	if sel := d.selectedRow(); sel != nil {
		d.scrollview.EnsureLineVisible(sel.FirstLine)
	}

	// Position the scrollview so its internal hit-test lines up with screen Y.
	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+2+sessionTreeHeaderLines)

	parts := []string{
		styles.DialogTitleStyle.Width(contentWidth).Render("Session Tree"),
		RenderSeparator(contentWidth),
		"",
		d.scrollview.View(),
		"",
		d.renderFooter(regionWidth),
	}
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)

	height := min(maxHeight, visible+sessionTreeHeaderLines+sessionTreeFooterLines+sessionTreeChrome)
	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Height(height).MaxHeight(height).Render(content)
}

// renderFooter combines the glyph legend and the help key line in the footer.
// The legend doubles as documentation for the shared vocabulary used in the
// sidebar and transcript rows.
func (d *sessionTreeDialog) renderFooter(contentWidth int) string {
	return RenderHelpKeys(contentWidth,
		"↑↓", "navigate",
		"enter", "open",
		"esc", "close",
	)
}

// selectedRow returns the currently selected row, or nil when rows is empty.
func (d *sessionTreeDialog) selectedRow() *sessionTreeRow {
	if d.selected < 0 || d.selected >= len(d.rows) {
		return nil
	}
	return &d.rows[d.selected]
}

// resolveSelection clamps d.selected to the current rows slice and prefers the
// row matching selectedNodeID. This keeps the same node highlighted across
// rebuilds (resize, re-render) and ensures d.selected is always a valid index
// when rows is non-empty.
func (d *sessionTreeDialog) resolveSelection() int {
	if len(d.rows) == 0 {
		return 0
	}
	if d.selectedNodeID != "" {
		for i, r := range d.rows {
			if r.Node.SessionID == d.selectedNodeID {
				return i
			}
		}
	}
	if d.selected < 0 {
		return 0
	}
	if d.selected >= len(d.rows) {
		return len(d.rows) - 1
	}
	return d.selected
}

// moveSelection shifts the selection by delta, clamped to the row range. It
// also updates selectedNodeID so the next rebuild keeps the same row.
func (d *sessionTreeDialog) moveSelection(delta int) {
	if len(d.rows) == 0 {
		return
	}
	idx := d.selected + delta
	idx = max(idx, 0)
	if idx >= len(d.rows) {
		idx = len(d.rows) - 1
	}
	d.selected = idx
	d.selectedNodeID = d.rows[idx].Node.SessionID
}

// openSelected emits the commands needed to navigate to the selected session.
// OpenSubAgentTabMsg's handler already knows how to switch to an existing tab,
// attach a new live tab, or surface a friendly info notice, so we always
// dispatch it regardless of whether the node is the root or a subagent.
func (d *sessionTreeDialog) openSelected() (layout.Model, tea.Cmd) {
	sel := d.selectedRow()
	if sel == nil {
		return d, nil
	}
	sessionID := sel.Node.SessionID
	if sessionID == "" {
		return d, nil
	}
	return d, tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(messages.OpenSubAgentTabMsg{SessionID: sessionID}),
	)
}

// hitTestRow translates a screen-Y coordinate into a row index, or -1 when the
// click did not land on a selectable row. It respects scroll offset so that
// rows scrolled out of view cannot be "clicked through".
func (d *sessionTreeDialog) hitTestRow(screenY int) int {
	dialogRow, _ := d.Position()
	listStart := dialogRow + 2 + sessionTreeHeaderLines
	visible := d.scrollview.VisibleHeight()
	if screenY < listStart || screenY >= listStart+visible {
		return -1
	}
	line := d.scrollview.ScrollOffset() + (screenY - listStart)
	for i, r := range d.rows {
		if line >= r.FirstLine && line <= r.LastLine {
			return i
		}
	}
	return -1
}

// applySelectionHighlight wraps a rendered tree line in a subtle highlight so
// the active selection reads clearly even on dense trees. We strip the inner
// ANSI first so the background applies uniformly across mixed badge + muted
// glyph styles; the content's original foreground would otherwise leak.
func applySelectionHighlight(line string, contentWidth int) string {
	plain := ansi.Strip(line)
	if contentWidth > 0 && ansi.StringWidth(plain) < contentWidth {
		plain += strings.Repeat(" ", contentWidth-ansi.StringWidth(plain))
	}
	return styles.SelectionStyle.Render(plain)
}

// --- Pure helpers (tested directly) ---------------------------------------

// buildSessionTreeLines is a thin adaptor kept for tests and callers that
// don't need the per-row mapping. It returns only the rendered lines.
// rootSessionID is fixed to "root-1" today; callers that need a different
// root should use buildSessionTree directly.
func buildSessionTreeLines(nodes []runtime.LiveSessionNode, currentSessionID string, renderedAt time.Time) []string {
	lines, _ := buildSessionTree(nodes, "root-1", currentSessionID, renderedAt, 80)
	return lines
}

// buildSessionTree renders the supplied nodes as a tree. It returns:
//   - the flat list of display lines (with styling baked in), and
//   - a row table so callers can map line indices back to nodes (for click /
//     selection handling).
//
// The function is pure: no I/O, no goroutines, no access to the runtime or
// the sidebar. That makes it cheap to test with hand-crafted node lists
// covering nested / orphaned / empty tree cases.
func buildSessionTree(nodes []runtime.LiveSessionNode, rootSessionID, currentSessionID string, renderedAt time.Time, contentWidth int) ([]string, []sessionTreeRow) {
	if len(nodes) == 0 {
		return nil, nil
	}
	if contentWidth <= 0 {
		contentWidth = 60
	}

	byID := make(map[string]runtime.LiveSessionNode, len(nodes))
	children := make(map[string][]runtime.LiveSessionNode)
	for _, n := range nodes {
		byID[n.SessionID] = n
	}
	// Children are indexed by their parent session id. A node is a "root"
	// of the tree we render when either its parent is outside the supplied
	// node list (orphaned attached tree) or when its session id matches the
	// provided rootSessionID.
	for _, n := range nodes {
		if n.SessionID == rootSessionID {
			continue
		}
		if _, ok := byID[n.ParentSessionID]; ok {
			children[n.ParentSessionID] = append(children[n.ParentSessionID], n)
		}
	}

	// Determine the ordered list of roots.
	var roots []runtime.LiveSessionNode
	if root, ok := byID[rootSessionID]; ok {
		roots = []runtime.LiveSessionNode{root}
	}
	for _, n := range nodes {
		if n.SessionID == rootSessionID {
			continue
		}
		if _, hasParent := byID[n.ParentSessionID]; hasParent {
			continue
		}
		roots = append(roots, n)
	}

	// Sort siblings by creation time so the tree reads chronologically. If
	// timestamps are missing we fall back to agent name.
	sortSiblings := func(list []runtime.LiveSessionNode) {
		sort.SliceStable(list, func(i, j int) bool {
			ti, tj := list[i].CreatedAt, list[j].CreatedAt
			if !ti.IsZero() && !tj.IsZero() && !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return list[i].AgentName < list[j].AgentName
		})
	}
	sortSiblings(roots)
	for parent := range children {
		siblings := children[parent]
		sortSiblings(siblings)
		children[parent] = siblings
	}

	var out []string
	var rows []sessionTreeRow

	var walk func(node runtime.LiveSessionNode, ancestorsHaveMore []bool, isLast, isRoot bool)
	walk = func(node runtime.LiveSessionNode, ancestorsHaveMore []bool, isLast, isRoot bool) {
		prefix := ""
		prefix2 := ""
		if !isRoot {
			prefix = treePrefix(ancestorsHaveMore, isLast, false)
			prefix2 = treePrefix(ancestorsHaveMore, isLast, true)
		}
		firstLine := len(out)
		out = append(out, renderSessionTreeLine(prefix, node, node.SessionID == currentSessionID, renderedAt, contentWidth))
		lastLine := firstLine
		if detail := renderSessionTreeDetailLine(prefix2, node, contentWidth); detail != "" {
			out = append(out, detail)
			lastLine = len(out) - 1
		}
		rows = append(rows, sessionTreeRow{Node: node, FirstLine: firstLine, LastLine: lastLine})

		kids := children[node.SessionID]
		for i, kid := range kids {
			kidIsLast := i == len(kids)-1
			next := append(append([]bool{}, ancestorsHaveMore...), !isLast)
			walk(kid, next, kidIsLast, false)
		}
	}
	for i, root := range roots {
		rootIsLast := i == len(roots)-1
		walk(root, nil, rootIsLast, true)
	}
	return out, rows
}

// treePrefix returns the ASCII branch prefix for a node at the given
// ancestor-has-more bitmap + isLast flag.
//
//	ancestorsHaveMore[i] == true  -> a vertical stem "│  " is drawn at depth i
//	                                 because the ancestor at that depth has
//	                                 more siblings below us.
//	ancestorsHaveMore[i] == false -> empty indentation "   " at that depth.
//
// detail=true returns the prefix used for the second (muted) line under a
// node: no branch connector, just stems + one indent column so follow-up
// nodes visually sit inside the tree shape.
func treePrefix(ancestorsHaveMore []bool, isLast, detail bool) string {
	var b strings.Builder
	for _, more := range ancestorsHaveMore {
		if more {
			b.WriteString("│  ")
		} else {
			b.WriteString("   ")
		}
	}
	if detail {
		// Second-line prefix sits directly under the first line's connector,
		// using a vertical stem only when this node is not the last sibling
		// (because the next sibling still needs the stem beneath it).
		if isLast {
			b.WriteString("   ")
		} else {
			b.WriteString("│  ")
		}
		return b.String()
	}
	if isLast {
		b.WriteString("└─ ")
	} else {
		b.WriteString("├─ ")
	}
	return b.String()
}

// renderSessionTreeLine formats the primary line for a node: tree prefix +
// glyph + agent badge + short session ref + optional current marker + right
// aligned status chip.
func renderSessionTreeLine(prefix string, node runtime.LiveSessionNode, isCurrent bool, renderedAt time.Time, contentWidth int) string {
	muted := styles.MutedStyle
	prefixStyled := muted.Render(prefix)
	glyph := sessionTreeGlyph(node)
	agent := strings.TrimSpace(node.AgentName)
	if agent == "" {
		if node.Kind == runtime.LiveSessionRoot {
			agent = "root"
		} else {
			agent = "subagent"
		}
	}
	badge := styles.AgentBadgeStyleFor(agent).Render(agent)

	var ref string
	if node.Kind != runtime.LiveSessionRoot && node.SessionID != "" {
		ref = muted.Render(" · " + subagent.ShortRef(node.SessionID))
	}

	left := prefixStyled + muted.Render(glyph) + " " + badge + ref
	if isCurrent {
		left += " " + styles.TabAccentStyle.Render("← you are here")
	}

	// Right side: status chip + relative age. Age is derived from CreatedAt
	// when available; falls back to LastUpdateAt; if both are zero we just
	// show the status chip so the line isn't misleading.
	statusText := sessionTreeStatusLabel(node)
	age := ""
	switch {
	case !node.CreatedAt.IsZero():
		age = sessionTreeAge(renderedAt, node.CreatedAt)
	case !node.LastUpdateAt.IsZero():
		age = sessionTreeAge(renderedAt, node.LastUpdateAt)
	}
	right := sessionTreeStatusStyle(node).Render(statusText)
	if age != "" {
		right += muted.Render("  " + age)
	}

	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	gap := contentWidth - leftWidth - rightWidth
	if gap < 1 {
		// Not enough room to right-align. Keep the primary info and drop
		// the age so the line still fits.
		right = sessionTreeStatusStyle(node).Render(statusText)
		rightWidth = lipgloss.Width(right)
		gap = contentWidth - leftWidth - rightWidth
	}
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// renderSessionTreeDetailLine returns an optional muted second line with
// title / preview / error, or "" if there's nothing extra to show.
func renderSessionTreeDetailLine(prefix string, node runtime.LiveSessionNode, contentWidth int) string {
	detail := ""
	switch {
	case strings.TrimSpace(node.Error) != "":
		detail = strings.TrimSpace(node.Error)
	case strings.TrimSpace(node.Title) != "" && !strings.EqualFold(node.Title, node.AgentName):
		detail = strings.TrimSpace(node.Title)
	case strings.TrimSpace(node.LastPreview) != "":
		detail = strings.TrimSpace(node.LastPreview)
	}
	if detail == "" {
		return ""
	}

	prefixStyled := styles.MutedStyle.Render(prefix)
	maxDetail := max(contentWidth-ansi.StringWidth(ansi.Strip(prefixStyled)), 10)
	text := toolcommon.TruncateText(detail, maxDetail)
	if strings.TrimSpace(node.Error) != "" {
		return prefixStyled + styles.ErrorStyle.Render(text)
	}
	return prefixStyled + styles.MutedStyle.Render(text)
}

// sessionTreeGlyph picks the leading glyph for a node row. The intent is that
// at a glance users can distinguish "root of the tree", "live subagent",
// "closed / stopped / failed subagent". The glyphs mirror those used in the
// sidebar + transcript so the mental model stays consistent across surfaces.
func sessionTreeGlyph(node runtime.LiveSessionNode) string {
	if node.Kind == runtime.LiveSessionRoot {
		return "●"
	}
	switch strings.ToLower(strings.TrimSpace(node.Status)) {
	case "closed":
		return "◇"
	case "stopped":
		return "■"
	case "failed":
		return "!"
	case "starting", "running":
		return "▶"
	case "waiting":
		return "◦"
	default:
		return "·"
	}
}

// sessionTreeStatusLabel maps raw subagent.Status strings to a compact
// user-facing label. Root sessions always read as "working" — this dialog is
// only ever opened while a session is alive, so the root is implicitly live.
func sessionTreeStatusLabel(node runtime.LiveSessionNode) string {
	if node.Kind == runtime.LiveSessionRoot {
		return "working"
	}
	switch strings.ToLower(strings.TrimSpace(node.Status)) {
	case "waiting":
		return "idle"
	case "closed":
		return "finalized"
	case "stopped":
		return "ended"
	case "failed":
		return "failed"
	case "starting", "running":
		return "working"
	default:
		if node.Status == "" {
			return "unknown"
		}
		return node.Status
	}
}

// sessionTreeStatusStyle picks a foreground style matching the status label so
// failed/ended entries stand out and live entries share the highlight tone the
// sidebar uses.
func sessionTreeStatusStyle(node runtime.LiveSessionNode) lipgloss.Style {
	switch strings.ToLower(strings.TrimSpace(node.Status)) {
	case "failed":
		return styles.ErrorStyle
	case "stopped":
		return styles.WarningStyle
	case "waiting", "closed":
		return styles.TabPrimaryStyle
	case "starting", "running":
		return styles.TabAccentStyle
	default:
		if node.Kind == runtime.LiveSessionRoot {
			return styles.TabAccentStyle
		}
		return styles.MutedStyle
	}
}

// sessionTreeAge formats "x ago" in a way that matches the sidebar's hover
// badge, so users see the same vocabulary for the same concept.
func sessionTreeAge(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	d = max(d, 0)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
