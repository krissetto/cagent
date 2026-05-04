package sidebar

import "time"

// sidebarSessionRow is the shared sidebar-facing representation of any
// attachable / session-like row we render as part of a tree or list.
//
// It intentionally contains only the data the sidebar needs to render and hit
// test a row — not the full runtime/session model. Agents and subagents adapt
// into this shape so rendering/click-zone logic can be shared instead of each
// section reinventing its own mini row model.
type sidebarSessionRow struct {
	ID              string
	DisplayName     string
	ParentID        string
	Depth           int
	IsCurrent       bool
	IsAttachable    bool
	StatusText      string
	Description     string
	Provider        string
	Model           string
	Preview         string
	UpdatedAt       time.Time
	CreatedAt       time.Time
	TreePrefix      string
	TrailingHint    string
	LeadingGlyph    string
	LeadingAnimated bool
}

// rowRenderPlan describes how a single sidebar session row is laid out in the
// rendered text. contentLines counts visible lines that should map back to the
// row's id for click hit testing; separatorLines counts blank lines that
// follow the row before the next one (skipped from the click-zone map).
type rowRenderPlan struct {
	row            sidebarSessionRow
	contentLines   int
	separatorLines int
}
