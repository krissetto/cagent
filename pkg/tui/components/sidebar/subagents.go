package sidebar

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// subagentState tracks a live runtime-managed subagent for sidebar display.
//
// The state is keyed by the subagent's full session id. The short ref shown to
// the model (and the user) is derived on the fly via subagent.ShortRef so the
// display always stays in sync with the identifier callers actually use.
type subagentState struct {
	ID              string
	ParentSessionID string
	AgentName       string
	Status          subagent.Status
	Preview         string
	// CreatedAt captures when the parent originally delegated to this
	// subagent. It is the wall-clock time the sidebar shows on hover so the
	// user can see "how long has this child been alive" rather than "how
	// long since its last turn". Populated from the runtime handle snapshot
	// on the started event; otherwise falls back to the first envelope time
	// or the moment the sidebar first observes the subagent.
	CreatedAt time.Time
	// UpdatedAt is the most recent activity timestamp. Used for sort order
	// (most recently touched agent on top) — NOT for the hover badge.
	UpdatedAt time.Time
}

// upsertSubagent canonicalizes any prior short-id duplicate for the given
// subagent reference, returns the authoritative state, and reports whether
// the entry was newly created on this call.
//
// All sidebar ingestion paths (live runtime events, live-tree seeding, and
// transcript restore) funnel through this helper so id canonicalization and
// fresh-vs-existing detection cannot diverge between callers. Callers are
// still responsible for applying their domain-specific merge rules afterward
// (status, agent name, parent linkage, timestamps, ...).
//
// Timestamp policy is intentionally NOT centralized here because the four
// ingestion paths each have legitimately different rules: a subagent_started
// event always overrides CreatedAt with the runtime's authoritative
// delegation time, an envelope update only backfills CreatedAt if missing,
// `sent` events do not touch CreatedAt at all, and the restore path only
// fills both timestamps when zero. Forcing a single helper would silently
// change one or more of those behaviors.
func (m *model) upsertSubagent(id string) *subagentState {
	if id == "" {
		return nil
	}
	if m.subagents == nil {
		m.subagents = make(map[string]*subagentState)
	}
	m.canonicalizeSubagentEntry(id)
	return m.getOrCreateSubAgent(id)
}

// recordSubAgentStart registers a new subagent as "starting".
func (m *model) recordSubAgentStart(ev *runtime.SubAgentStartedEvent) {
	if ev == nil || ev.SubAgent.ID == "" {
		return
	}
	snap := ev.SubAgent
	state := m.upsertSubagent(snap.ID)
	state.AgentName = snap.AgentName
	switch {
	case snap.ParentSessionID != "":
		state.ParentSessionID = snap.ParentSessionID
	case ev.SessionID != "":
		state.ParentSessionID = ev.SessionID
	}
	if snap.Status != 0 {
		state.Status = snap.Status
	} else {
		state.Status = subagent.StatusStarting
	}
	state.Preview = snap.LastPreview
	now := time.Now()
	// Prefer the runtime's real delegation time so the sidebar hover shows
	// when the subagent was actually started, not when we first saw the
	// event.
	if !snap.CreatedAt.IsZero() {
		state.CreatedAt = snap.CreatedAt
	} else if state.CreatedAt.IsZero() {
		state.CreatedAt = now
	}
	state.UpdatedAt = now
	m.invalidateCache()
}

// recordSubAgentSent marks the subagent as working again and bumps its
// timestamp so the list sort order reflects the most recent parent-side
// activity. Without this, a child that had just completed a turn would stay
// visually "idle" even after the parent sent it fresh work.
func (m *model) recordSubAgentSent(ev *runtime.SubAgentSentEvent) {
	if ev == nil || ev.SubAgentID == "" {
		return
	}
	state := m.upsertSubagent(ev.SubAgentID)
	// The parent's SessionID is carried on the event; backfill it if we
	// haven't seen a started/update event yet.
	if state.ParentSessionID == "" && ev.SessionID != "" {
		state.ParentSessionID = ev.SessionID
	}
	// Guard: if the subagent has already reached a terminal state
	// (StatusClosed/StatusStopped/StatusFailed), do not resurrect it back to
	// running. Terminal subagents cannot be re-sent to; receiving a stale or
	// out-of-order Sent event for one must not flip the sidebar back into a
	// "working" state and silently re-engage the spinner.
	if state.Status.IsTerminal() {
		return
	}
	// A send means the child has fresh work and should be shown as working
	// immediately, even before its next turn-completed envelope arrives.
	state.Status = subagent.StatusRunning
	state.UpdatedAt = time.Now()
	m.invalidateCache()
}

// recordSubAgentUpdate applies a child envelope to the tracked state.
func (m *model) recordSubAgentUpdate(ev *runtime.SubAgentUpdateEvent) {
	if ev == nil || ev.Envelope.SubAgentID == "" {
		return
	}
	env := ev.Envelope
	state := m.upsertSubagent(env.SubAgentID)
	if env.AgentName != "" {
		state.AgentName = env.AgentName
	}
	if env.ParentSessionID != "" {
		state.ParentSessionID = env.ParentSessionID
	}
	state.Status = env.Status
	if env.Preview != "" {
		state.Preview = env.Preview
	}
	if env.Error != "" && env.Status == subagent.StatusFailed {
		state.Preview = env.Error
	}
	now := time.Now()
	var updatedAt time.Time
	if !env.At.IsZero() {
		updatedAt = env.At
	} else {
		updatedAt = now
	}
	// Back-fill CreatedAt for subagents we only started tracking via an
	// update (e.g. the observer attached after StartChild). We never
	// overwrite a non-zero CreatedAt because the started event's timestamp
	// is always more accurate than the first envelope we saw.
	if state.CreatedAt.IsZero() {
		state.CreatedAt = updatedAt
	}
	state.UpdatedAt = updatedAt
	m.invalidateCache()
}

// canonicalizeSubagentEntry reconciles a freshly-seen full session id with
// any previously-tracked entries for the same logical subagent.
//
// In normal operation the sidebar is now seeded solely from the runtime's
// canonical live-session tree, which always carries full session IDs. The
// short-id reconciliation remains as a defensive compatibility layer for any
// pre-existing state keyed by short refs and for tests that simulate older
// restore-era rows.
func (m *model) canonicalizeSubagentEntry(fullID string) *subagentState {
	if fullID == "" {
		return nil
	}
	if m.subagents == nil {
		m.subagents = make(map[string]*subagentState)
	}
	shortID := subagent.ShortRef(fullID)

	live, hasLive := m.subagents[fullID]
	if shortID == fullID || shortID == "" {
		if hasLive {
			return live
		}
		return nil
	}

	stale, hasStale := m.subagents[shortID]
	if !hasStale {
		return live
	}
	if !hasLive {
		// Promote the restored entry by re-keying it under the full id; this
		// preserves whatever the restore path had captured (agent name,
		// parent linkage if present) while letting subsequent live updates
		// land on the same row.
		delete(m.subagents, shortID)
		stale.ID = fullID
		m.subagents[fullID] = stale
		return stale
	}
	// Both keys exist. Keep the live entry (authoritative) and carry over any
	// agent name from the stale restore entry if the live one is missing it.
	if strings.TrimSpace(live.AgentName) == "" && strings.TrimSpace(stale.AgentName) != "" {
		live.AgentName = stale.AgentName
	}
	delete(m.subagents, shortID)
	return live
}

// SeedSubagentsFromLiveTree primes the Subagents section with the runtime's
// current live-tree snapshot. It is intended to be called once, right after a
// chat page is created for an attached child-session tab: the sidebar is
// otherwise event-driven, so attaching to a session whose subagents already
// started running would leave the sidebar empty until each child emitted
// another update. For owned root tabs it is also a safe no-op because the
// event path still drives all subsequent updates.
//
// Nodes that are not LiveSessionSubAgent (e.g. the root node returned by
// [runtime.LocalRuntime.LiveSessionTree]) are ignored. The method is
// idempotent: re-seeding with the same tree leaves the sidebar in the same
// state because [getOrCreateSubAgent] reuses existing entries.
func (m *model) SeedSubagentsFromLiveTree(nodes []runtime.LiveSessionNode) {
	if len(nodes) == 0 {
		return
	}
	for _, n := range nodes {
		if n.Kind != runtime.LiveSessionSubAgent || n.SessionID == "" {
			continue
		}
		state := m.upsertSubagent(n.SessionID)
		if strings.TrimSpace(n.AgentName) != "" || state.AgentName == "" {
			state.AgentName = n.AgentName
		}
		if strings.TrimSpace(n.ParentSessionID) != "" {
			state.ParentSessionID = n.ParentSessionID
		}

		// Compute the timestamp the snapshot claims for this entry. We need
		// it for the recency guard below and as the new UpdatedAt value when
		// we do apply the snapshot.
		snapshotUpdatedAt := n.LastUpdateAt
		if snapshotUpdatedAt.IsZero() {
			snapshotUpdatedAt = n.CreatedAt
		}

		// Guards: protect Status/UpdatedAt/Preview from being regressed by
		// the snapshot when:
		//   (a) the existing entry is already in a terminal state — once a
		//       subagent has closed/stopped/failed in the sidebar, a snapshot
		//       must never flip it back to a non-terminal state; and
		//   (b) the existing entry's UpdatedAt is more recent than the
		//       snapshot's — in that case the event-driven path has already
		//       written fresher data than this snapshot carries.
		//
		// Fresh entries created moments earlier by upsertSubagent have a
		// zero UpdatedAt, so guard (b) correctly does not fire for them.
		existingIsNewer := !state.UpdatedAt.IsZero() && state.UpdatedAt.After(snapshotUpdatedAt)
		if !state.Status.IsTerminal() && !existingIsNewer {
			snapshotStatus := statusFromLiveNode(n.Status)
			state.Status = snapshotStatus
			preview := strings.TrimSpace(n.LastPreview)
			if n.Error != "" && snapshotStatus == subagent.StatusFailed {
				preview = strings.TrimSpace(n.Error)
			}
			if preview != "" {
				state.Preview = preview
			}
			updated := snapshotUpdatedAt
			if updated.IsZero() {
				updated = state.CreatedAt
			}
			state.UpdatedAt = updated
		}

		if !n.CreatedAt.IsZero() {
			state.CreatedAt = n.CreatedAt
		} else if state.CreatedAt.IsZero() {
			state.CreatedAt = time.Now()
		}
	}
	m.invalidateCache()
}

// statusFromLiveNode maps a [runtime.LiveSessionNode.Status] string back to
// the [subagent.Status] the sidebar keeps internally. The string form comes
// straight from [subagent.Status.String], so the mapping is exact; unknown
// values fall back to [subagent.StatusRunning] because a tracked descendant
// that reports an unknown status is definitely not terminal from the
// sidebar's perspective.
func statusFromLiveNode(status string) subagent.Status {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "starting":
		return subagent.StatusStarting
	case "running":
		return subagent.StatusRunning
	case "waiting":
		return subagent.StatusWaiting
	case "closed":
		return subagent.StatusClosed
	case "stopped":
		return subagent.StatusStopped
	case "failed":
		return subagent.StatusFailed
	default:
		return subagent.StatusRunning
	}
}

// syncSubagentSpinner keeps the shared sidebar spinner ticking while there is
// at least one live subagent to render, and lets it wind down once every
// tracked subagent has reached a terminal state. Returns a command to start
// the spinner's animation subscription when applicable; safe to call on
// every subagent-event dispatch.
func (m *model) syncSubagentSpinner() tea.Cmd {
	if m.liveSubagentCount() > 0 {
		return m.startSpinner()
	}
	// stopSpinner respects needsSpinner() so this only actually stops when no
	// other sidebar state still needs the shared spinner.
	m.stopSpinner()
	return nil
}

// setHoveredSubagent updates the currently-hovered subagent id. Returns true
// when the hover state actually changed so the caller can invalidate caches.
func (m *model) setHoveredSubagent(id string) bool {
	if m.hoveredSubagentID == id {
		return false
	}
	m.hoveredSubagentID = id
	return true
}

func (m *model) getOrCreateSubAgent(id string) *subagentState {
	if m.subagents == nil {
		m.subagents = make(map[string]*subagentState)
	}
	if existing, ok := m.subagents[id]; ok {
		return existing
	}

	// Canonicalise restore/live duplicates in both directions.
	//
	// If the caller asks for a short id but a full-id row already exists with the
	// same short ref, reuse the full-id row.
	//
	// If the caller asks for a full id and there is already a stale short-id row,
	// promote that row to the full-id key.
	shortID := subagent.ShortRef(id)
	if shortID != "" {
		if shortID == id {
			// Short-id lookup. Reuse an existing full-id row if present.
			for existingID, existing := range m.subagents {
				if existingID != id && subagent.ShortRef(existingID) == id {
					return existing
				}
			}
		} else if stale, ok := m.subagents[shortID]; ok {
			delete(m.subagents, shortID)
			stale.ID = id
			m.subagents[id] = stale
			return stale
		}
	}

	now := time.Now()
	state := &subagentState{
		ID:        id,
		Status:    subagent.StatusStarting,
		CreatedAt: now,
		// UpdatedAt is intentionally left zero on creation so the
		// SeedSubagentsFromLiveTree recency guard can tell apart a fresh
		// placeholder entry (no real activity yet) from one whose UpdatedAt
		// has actually been advanced by a live event. Callers that observe a
		// real update (start/sent/update/seed) write their own UpdatedAt.
	}
	m.subagents[id] = state
	return state
}

// subagentEntries returns the sidebar's subagent states in tree order.
//
// Roots are the direct children of the current/root session (their parent is
// not another tracked subagent). Each node is followed by its descendants in
// depth-first order. Siblings are sorted by initial delegation time, newest
// first. This ordering is intentionally based on CreatedAt rather than
// UpdatedAt so incidental state changes (hover, updates, status changes) never
// cause rows to jump around under the user's cursor.
func (m *model) subagentEntries() []*subagentState {
	if len(m.subagents) == 0 {
		return nil
	}

	children := make(map[string][]*subagentState)
	var roots []*subagentState
	for _, s := range m.subagents {
		if parent := s.ParentSessionID; parent != "" {
			if _, ok := m.subagents[parent]; ok {
				children[parent] = append(children[parent], s)
				continue
			}
		}
		roots = append(roots, s)
	}

	sortSubagentSiblings(roots)
	for parentID := range children {
		sortSubagentSiblings(children[parentID])
	}

	out := make([]*subagentState, 0, len(m.subagents))
	var walk func(nodes []*subagentState)
	walk = func(nodes []*subagentState) {
		for _, node := range nodes {
			out = append(out, node)
			if childNodes := children[node.ID]; len(childNodes) > 0 {
				walk(childNodes)
			}
		}
	}
	walk(roots)
	return out
}

// sortSubagentSiblings orders entries by initial delegation time, newest
// first. The order must not depend on UpdatedAt: updates, status changes,
// hover refreshes, and other incidental UI state should never make rows jump
// around while the user is moving the mouse through the sidebar.
//
// When two entries share the exact same CreatedAt — which happens after a
// session reload because the persistent store rounds timestamps to second
// precision (time.RFC3339) — siblings are tie-broken alphabetically by agent
// name. Without this, the surrounding map-iteration order leaks through
// sort.SliceStable and the rows reshuffle on every re-render.
func sortSubagentSiblings(entries []*subagentState) {
	sort.SliceStable(entries, func(i, j int) bool {
		ai, aj := entries[i].CreatedAt, entries[j].CreatedAt
		if ai.Equal(aj) {
			if entries[i].AgentName == entries[j].AgentName {
				return entries[i].ID < entries[j].ID
			}
			return entries[i].AgentName < entries[j].AgentName
		}
		return ai.After(aj)
	})
}

// liveSubagentCount returns the number of non-terminal subagents.
func (m *model) liveSubagentCount() int {
	var n int
	for _, s := range m.subagents {
		if !s.Status.IsTerminal() {
			n++
		}
	}
	return n
}

// markAllAsTerminal transitions every non-terminal subagent to StatusClosed
// and bumps its UpdatedAt to now. It is intended to be called when the parent
// stream is cancelled (ESC): without this, in-flight subagent rows would keep
// the sidebar spinner ticking and continue rendering the running glyph even
// though no live work is happening behind them anymore.
//
// The entries themselves are intentionally preserved (not deleted) so the
// user can still see what was running at cancellation time. Use
// [model.evictTerminal] to remove old terminal rows when desired.
func (m *model) markAllAsTerminal() {
	now := time.Now()
	changed := false
	for _, s := range m.subagents {
		if !s.Status.IsTerminal() {
			s.Status = subagent.StatusClosed
			s.UpdatedAt = now
			changed = true
		}
	}
	if changed {
		m.invalidateCache()
	}
}

// evictTerminal removes subagent entries that have reached a terminal status
// (StatusClosed/StatusStopped/StatusFailed) and whose UpdatedAt is older than
// olderThan. It is a maintenance helper to keep the subagents map from
// growing without bound across long-running sessions; the sidebar does not
// invoke it automatically — callers decide the cadence.
//
//nolint:unused // Available for future callers; the unbounded-growth issue is documented.
func (m *model) evictTerminal(olderThan time.Duration) {
	if len(m.subagents) == 0 {
		return
	}
	cutoff := time.Now().Add(-olderThan)
	evicted := false
	for id, s := range m.subagents {
		if !s.Status.IsTerminal() {
			continue
		}
		// A zero UpdatedAt means the entry has never been touched by an
		// event; treat that as "old enough to evict" so stale placeholders
		// don't linger forever.
		if s.UpdatedAt.IsZero() || s.UpdatedAt.Before(cutoff) {
			delete(m.subagents, id)
			evicted = true
		}
	}
	if evicted {
		m.invalidateCache()
	}
}

// subagentDepth returns how many tracked subagent ancestors the given node has.
// Direct children of the root session have depth 0, grandchildren depth 1, etc.
func (m *model) subagentDepth(s *subagentState) int {
	if s == nil {
		return 0
	}
	depth := 0
	seen := map[string]bool{s.ID: true}
	for parentID := s.ParentSessionID; parentID != ""; {
		parent, ok := m.subagents[parentID]
		if !ok {
			return depth
		}
		depth++
		if seen[parent.ID] {
			return depth // defensive cycle break
		}
		seen[parent.ID] = true
		parentID = parent.ParentSessionID
	}
	return depth
}

// subagentRenderEntry augments a [subagentState] with the tree-drawing
// context the sidebar needs to render proper sibling connectors (├ / └)
// and ancestor stems (│). It is computed once per render from
// [subagentEntries] so the rendering path itself stays simple.
type subagentRenderEntry struct {
	state             *subagentState
	depth             int
	isLast            bool
	ancestorsHaveMore []bool
}

// subagentRenderEntries returns the tree in DFS order along with, for each
// entry, the booleans needed to draw a proper tree branch prefix.
//
// Most sidebar consumers only care about the "flat in DFS order" view, which
// is what [subagentEntries] already returns; this version adds the metadata
// needed by [renderSubagentEntry] to emit connectors that distinguish the
// last sibling from earlier siblings and carry ancestor stems down through
// nested children. Keeping the metadata-free API around avoids churn for
// existing callers (tests, click-zone builders).
func (m *model) subagentRenderEntries() []subagentRenderEntry {
	if len(m.subagents) == 0 {
		return nil
	}

	children := make(map[string][]*subagentState)
	var roots []*subagentState
	for _, s := range m.subagents {
		if parent := s.ParentSessionID; parent != "" {
			if _, ok := m.subagents[parent]; ok {
				children[parent] = append(children[parent], s)
				continue
			}
		}
		roots = append(roots, s)
	}

	sortSubagentSiblings(roots)
	for parentID := range children {
		sortSubagentSiblings(children[parentID])
	}

	out := make([]subagentRenderEntry, 0, len(m.subagents))
	var walk func(nodes []*subagentState, ancestorsHaveMore []bool, depth int)
	walk = func(nodes []*subagentState, ancestorsHaveMore []bool, depth int) {
		for i, node := range nodes {
			isLast := i == len(nodes)-1
			out = append(out, subagentRenderEntry{
				state:             node,
				depth:             depth,
				isLast:            isLast,
				ancestorsHaveMore: append([]bool{}, ancestorsHaveMore...),
			})
			if kids := children[node.ID]; len(kids) > 0 {
				walk(kids, append(append([]bool{}, ancestorsHaveMore...), !isLast), depth+1)
			}
		}
	}
	walk(roots, nil, 0)
	return out
}

// subagentTreePrefix renders the ASCII branch prefix for a sidebar row at the
// given depth / sibling context. The `detail` flag returns the prefix used
// beneath the row (e.g. for the failed-preview line) so it sits visually
// inside the tree shape instead of breaking out of it.
func subagentTreePrefix(ancestorsHaveMore []bool, depth int, isLast, detail bool) string {
	if depth == 0 {
		return ""
	}
	var b strings.Builder
	for _, more := range ancestorsHaveMore {
		if more {
			b.WriteString("│ ")
		} else {
			b.WriteString("  ")
		}
	}
	if detail {
		if isLast {
			b.WriteString("  ")
		} else {
			b.WriteString("│ ")
		}
		return b.String()
	}
	if isLast {
		b.WriteString("└ ")
	} else {
		b.WriteString("├ ")
	}
	return b.String()
}

// subagentSection renders the "Subagents (N)" tab for the sidebar.
// Returns "" when there are no known subagents.
func (m *model) subagentSection(contentWidth int) string {
	entries := m.subagentRenderEntries()
	if len(entries) == 0 {
		return ""
	}

	maxWidth := contentWidth - treePrefixWidth
	if maxWidth <= 0 {
		maxWidth = contentWidth
	}

	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		m.renderSubagentEntry(&b, e, contentWidth, maxWidth)
	}

	title := fmt.Sprintf("Subagents (%d)", len(entries))
	if live := m.liveSubagentCount(); live > 0 && live != len(entries) {
		title = fmt.Sprintf("Subagents (%d live / %d)", live, len(entries))
	}
	return m.renderTab(title, b.String(), contentWidth)
}

// renderSubagentEntry writes a two-line block for a single subagent, using
// the tree-drawing context carried on the entry.
func (m *model) renderSubagentEntry(b *strings.Builder, e subagentRenderEntry, contentWidth, maxWidth int) {
	s := e.state
	headerStyle := styles.AgentAccentStyleFor(s.AgentName)
	if m.hoveredSubagentID == s.ID {
		headerStyle = styles.Hovered(headerStyle)
	}
	terminal := s.Status.IsTerminal()

	// Glyph reflects lifecycle: spinner while running/starting, terminal
	// glyph for finalized/stopped/failed, neutral middle-dot otherwise. The
	// arrow glyph (▶) is intentionally reserved for the "current agent"
	// row in the Agents section — the Subagents section must never reuse it,
	// because that visual overlap reads as a stale selection indicator on
	// the wrong row.
	var glyph string
	switch {
	case terminal:
		glyph = terminalGlyph(s.Status)
	case s.Status == subagent.StatusRunning || s.Status == subagent.StatusStarting:
		glyph = m.spinner.RawFrame()
	default:
		glyph = "·"
	}

	agentName := s.AgentName
	if agentName == "" {
		agentName = "subagent"
	}

	prefix := styles.MutedStyle.Render(subagentTreePrefix(e.ancestorsHaveMore, e.depth, e.isLast, false))

	// Left side: prefix + glyph + agent name + short ref.
	left := prefix + headerStyle.Render(glyph) + " " + headerStyle.Render(agentName) +
		styles.MutedStyle.Render(" · "+subagent.ShortRef(s.ID))
	// Right side: status label OR hover-relative age (mutually exclusive).
	// Hovering a row swaps the status chip for a compact "Xm ago"
	// reading off the original delegation time — that tells the user how
	// long this subagent session has been alive, independent of when it
	// last produced output.
	var right string
	if m.hoveredSubagentID == s.ID {
		right = styles.MutedStyle.Render(formatRelativeAge(s.CreatedAt, time.Now()))
	} else {
		statusText := statusLabel(s.Status)
		statusStyle := statusStyleFor(s.Status)
		right = statusStyle.Render(statusText)
	}

	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	spaceWidth := max(contentWidth-leftWidth-rightWidth, 1)
	b.WriteString(left + strings.Repeat(" ", spaceWidth) + right)

	if s.Preview != "" && s.Status == subagent.StatusFailed {
		// Only render a preview line for failed subagents — there it carries
		// the diagnostic error message, which the user actually wants to see.
		// Successful turn previews are deliberately hidden: they are raw
		// response content and leaking them to the sidebar duplicates what
		// the parent agent is already integrating into its own context.
		b.WriteString("\n")
		previewStyle := styles.ErrorStyle
		detailPrefix := subagentTreePrefix(e.ancestorsHaveMore, e.depth, e.isLast, true)
		previewPrefix := styles.MutedStyle.Render(detailPrefix)
		previewPrefix += styles.MutedStyle.Render("└ ")
		b.WriteString(previewPrefix)
		previewWidth := max(10, maxWidth-(2*max(e.depth, 0)))
		b.WriteString(previewStyle.Render(toolcommon.TruncateText(s.Preview, previewWidth)))
	}
}

func (m *model) subagentRowPlans() []rowRenderPlan {
	entries := m.subagentRenderEntries()
	if len(entries) == 0 {
		return nil
	}
	plans := make([]rowRenderPlan, 0, len(entries))
	for _, entry := range entries {
		state := entry.state
		contentLines := 1
		if state != nil && state.Preview != "" && state.Status == subagent.StatusFailed {
			contentLines = 2
		}
		plans = append(plans, rowRenderPlan{
			row: sidebarSessionRow{
				ID:           state.ID,
				DisplayName:  state.AgentName,
				ParentID:     state.ParentSessionID,
				Depth:        entry.depth,
				IsAttachable: true,
				Preview:      state.Preview,
				CreatedAt:    state.CreatedAt,
				UpdatedAt:    state.UpdatedAt,
				TreePrefix:   sessionTreePrefix(entry.depth, entry.isLast, entry.ancestorsHaveMore),
			},
			contentLines:   contentLines,
			separatorLines: 0,
		})
		_ = state // keep explicit in case future fields move around
	}
	return plans
}

// buildSubagentClickZones populates subagentClickZones by counting the exact
// number of rendered lines contributed by each subagent entry. We no longer
// rely on blank spacer lines between entries, so the mapping remains correct
// even when rows are rendered back-to-back.
func (m *model) buildSubagentClickZones(subagentSectionStart int, lines []string) {
	if m.subagentClickZones == nil {
		m.subagentClickZones = make(map[int]string)
	}
	buildRowClickZones(m.subagentClickZones, subagentSectionStart, lines, m.subagentRowPlans())
}

// subagentSummaryCollapsed returns a single-line summary of active subagents
// suitable for the collapsed sidebar. Returns "" when there are no subagents.
func (m *model) subagentSummaryCollapsed() string {
	if len(m.subagents) == 0 {
		return ""
	}
	live := m.liveSubagentCount()
	total := len(m.subagents)
	var label string
	switch live {
	case 0:
		label = fmt.Sprintf("Subagents: %d (idle)", total)
	case total:
		label = fmt.Sprintf("Subagents: %d live", live)
	default:
		label = fmt.Sprintf("Subagents: %d live / %d", live, total)
	}
	return styles.TabAccentStyle.Render("▦") + " " + styles.TabPrimaryStyle.Render(label)
}

func statusLabel(st subagent.Status) string {
	switch st {
	case subagent.StatusStarting, subagent.StatusRunning:
		// User wording: while a subagent is spinning up or actively running
		// we call it "working" rather than distinguishing between the two
		// internal substates. The spinner glyph still communicates liveness.
		return "working"
	case subagent.StatusWaiting:
		return "idle"
	case subagent.StatusClosed:
		return "finalized"
	case subagent.StatusStopped:
		return "ended"
	case subagent.StatusFailed:
		return "failed"
	default:
		return st.String()
	}
}

// formatRelativeAge renders a compact "x ago" string for hover badges.
// Buckets are chosen to read naturally at a glance:
//
//	< 5s          -> "just now"
//	< 60s         -> "Ns ago"
//	< 60m         -> "Nm ago"
//	< 24h         -> "Nh ago"
//	default       -> "Nd ago"
//
// A zero or future t degrades gracefully to "just now" so a badly-set clock
// never produces negative numbers in the sidebar.
func formatRelativeAge(t, now time.Time) string {
	if t.IsZero() {
		return "just now"
	}
	d := now.Sub(t)
	if d < 5*time.Second {
		return "just now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func statusStyleFor(st subagent.Status) lipgloss.Style {
	switch st {
	case subagent.StatusRunning, subagent.StatusStarting:
		return styles.TabAccentStyle
	case subagent.StatusWaiting:
		return styles.TabPrimaryStyle
	case subagent.StatusFailed:
		return styles.ErrorStyle
	case subagent.StatusStopped:
		return styles.WarningStyle
	default:
		return styles.MutedStyle
	}
}

func terminalGlyph(st subagent.Status) string {
	switch st {
	case subagent.StatusClosed:
		return "◦"
	case subagent.StatusStopped:
		return "■"
	case subagent.StatusFailed:
		return "!"
	default:
		return "•"
	}
}
