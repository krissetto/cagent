package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func testBreakdown() *runtime.ContextBreakdown {
	return &runtime.ContextBreakdown{
		SystemPrompt:      runtime.ContextCategory{Tokens: 1200, Items: 3},
		ToolDefinitions:   runtime.ContextCategory{Tokens: 8400, Items: 23},
		PromptFiles:       runtime.ContextCategory{Tokens: 600, Items: 1},
		Messages:          runtime.ContextCategory{Tokens: 6100, Items: 12},
		ToolResults:       runtime.ContextCategory{Tokens: 8200, Items: 18},
		CompactionSummary: runtime.ContextCategory{Tokens: 900, Items: 1},
		ContextLimit:      128_000,
		Model:             "openai/gpt-4o",
	}
}

func testBreakdownWithFiles() *runtime.ContextBreakdown {
	b := testBreakdown()
	b.AttachedFiles = []runtime.ContextFile{
		{Path: "/proj/main.go", Tokens: 2100},
		{Path: "/proj/docs/readme.md", Tokens: 300},
		{Path: "/tmp/deleted.txt", Missing: true},
	}
	b.PromptFileItems = []runtime.ContextFile{
		{Path: "/proj/AGENTS.md", Tokens: 600},
	}
	return b
}

func TestNewContextDialog(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdown())
	require.NotNil(t, dialog)
}

func TestContextDialogView(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdown())
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "Context Window")
	assert.Contains(t, view, "openai/gpt-4o")
	assert.Contains(t, view, "limit: 128.0K tokens")

	// Every category is listed, even implicitly small ones.
	assert.Contains(t, view, "System prompt")
	assert.Contains(t, view, "Tool definitions")
	assert.Contains(t, view, "Prompt files")
	assert.Contains(t, view, "Messages")
	assert.Contains(t, view, "Tool results")
	assert.Contains(t, view, "Compaction summary")
	assert.Contains(t, view, "Free space")

	// Item counts and the estimate disclaimer.
	assert.Contains(t, view, "(23 tools)")
	assert.Contains(t, view, "(12 messages)")
	assert.Contains(t, view, "(1 file)")
	assert.Contains(t, view, "estimates")

	// Usage summary: 25.4K used of 128K.
	assert.Contains(t, view, "~25.4K of 128.0K tokens")
}

func TestContextDialogViewUnknownLimit(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.ContextLimit = 0
	b.Model = "external-harness"

	dialog := NewContextDialog(b)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "context limit unknown")
	assert.Contains(t, view, "tokens estimated")
	assert.NotContains(t, view, "Free space")
}

// TestContextDialogViewCompactionCap verifies the dedicated second header
// line attributing a capped effective limit to the compaction model imposing
// it, shown ONLY when the compaction model's window is strictly smaller than
// the primary model's.
func TestContextDialogViewCompactionCap(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.ContextLimit = 16_000
	b.CompactionModel = "local/small"
	b.CompactionContextLimit = 16_000
	b.PrimaryContextLimit = 128_000

	dialog := NewContextDialog(b)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "limit: 16.0K tokens")
	assert.Contains(t, view, "compaction cap: local/small • 16.0K tokens")
	assert.NotContains(t, view, "128.0K", "the primary window is no longer echoed in the capped line")
}

// TestContextDialogViewCompactionModelNotCapping verifies the second header
// line stays absent when a dedicated compaction model is configured but its
// window is equal to or larger than the primary model's, i.e. it never caps.
func TestContextDialogViewCompactionModelNotCapping(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.CompactionModel = "local/big"
	b.CompactionContextLimit = 128_000
	b.PrimaryContextLimit = 128_000

	dialog := NewContextDialog(b)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "limit: 128.0K tokens")
	assert.NotContains(t, view, "compaction cap:")
}

// TestContextDialogViewCompactionOnlyResolvableLimit covers the edge case
// where only the compaction model's window is resolvable (the primary
// model's own window is unknown): the main meta line re-attributes the
// figure to the compaction model instead of silently presenting it as the
// primary model's, and the second (capped) line stays absent since there is
// no primary window to compare against.
func TestContextDialogViewCompactionOnlyResolvableLimit(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.ContextLimit = 16_000
	b.CompactionModel = "local/small"
	b.CompactionContextLimit = 16_000
	b.PrimaryContextLimit = 0

	dialog := NewContextDialog(b)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "limit: 16.0K tokens (from compaction model)")
	assert.NotContains(t, view, "compaction cap:")
}

func TestContextDialogEmptyBreakdown(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(&runtime.ContextBreakdown{Model: "openai/gpt-4o"})
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "Context Window")
	assert.Contains(t, view, "System prompt")
}

func TestContextRowsFreeSpace(t *testing.T) {
	t.Parallel()

	rows := contextRows(testBreakdown())
	require.Len(t, rows, 7)
	last := rows[len(rows)-1]
	assert.True(t, last.free)
	// 128000 - 25400 estimated tokens used.
	assert.Equal(t, int64(102_600), last.tokens)

	// An over-budget estimate must not produce a negative free-space row.
	over := testBreakdown()
	over.Messages.Tokens = 500_000
	rows = contextRows(over)
	require.Len(t, rows, 6)
	for _, row := range rows {
		assert.False(t, row.free)
	}
}

func TestContextPercentLabel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "-", percentLabel(0, 1000))
	assert.Equal(t, "-", percentLabel(10, 0))
	assert.Equal(t, "<1%", percentLabel(1, 1000))
	assert.Equal(t, "5%", percentLabel(50, 1000))
	assert.Equal(t, "100%", percentLabel(1000, 1000))
}

func TestContextScaleTokens(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	assert.Equal(t, int64(128_000), scaleTokens(b))

	// Estimate exceeding the limit scales against the estimate so the bar
	// and percentages stay consistent (never above 100%).
	b.Messages.Tokens = 500_000
	assert.Equal(t, b.TotalTokens(), scaleTokens(b))

	// Unknown limit scales against the estimated total.
	b = testBreakdown()
	b.ContextLimit = 0
	assert.Equal(t, b.TotalTokens(), scaleTokens(b))
}

func TestRenderContextBarWidth(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	rows := contextRows(b)

	for _, width := range []int{1, 10, 40, 80, 200} {
		bar := renderContextBar(rows, scaleTokens(b), width)
		assert.Equal(t, width, lipgloss.Width(bar), "bar must span exactly %d cells", width)
	}

	assert.Empty(t, renderContextBar(rows, 0, 40), "zero scale renders no bar")
	assert.Empty(t, renderContextBar(rows, scaleTokens(b), 0), "zero width renders no bar")
}

func TestContextDialogPlainText(t *testing.T) {
	t.Parallel()

	d := &contextDialog{breakdown: testBreakdown()}
	text := d.renderPlainText()

	assert.Contains(t, text, "Context Window")
	assert.Contains(t, text, "openai/gpt-4o")
	assert.Contains(t, text, "Tool definitions")
	assert.Contains(t, text, "8.4K")
	assert.Contains(t, text, "(23 tools)")
	assert.Contains(t, text, "Free space")
	assert.Contains(t, text, "estimates")
	assert.NotContains(t, text, "\x1b[", "plain text must carry no ANSI escapes")
}

// TestContextDialogPlainTextCompactionCap verifies the plain-text/clipboard
// path (used for copy) also carries the capped second line.
func TestContextDialogPlainTextCompactionCap(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.ContextLimit = 16_000
	b.CompactionModel = "local/small"
	b.CompactionContextLimit = 16_000
	b.PrimaryContextLimit = 128_000

	d := &contextDialog{breakdown: b}
	text := d.renderPlainText()

	assert.Contains(t, text, "compaction cap: local/small • 16.0K tokens")
	assert.NotContains(t, text, "\x1b[", "plain text must carry no ANSI escapes")
}

// ---------------------------------------------------------------------------
// File inventory (attached files, prompt files)
// ---------------------------------------------------------------------------

func TestContextDialogViewInventory(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdownWithFiles())
	dialog.SetSize(120, 50)
	view := dialog.View()

	assert.Contains(t, view, "Attached files")
	assert.Contains(t, view, "main.go")
	assert.Contains(t, view, "readme.md")
	assert.Contains(t, view, "deleted.txt")
	assert.Contains(t, view, "(missing)")
	assert.Contains(t, view, "Prompt files")
	assert.Contains(t, view, "AGENTS.md")
	assert.Contains(t, view, "drop", "help must advertise the drop key")
}

func TestContextDialogViewNoInventory(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdown())
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.NotContains(t, view, "Attached files")
	assert.NotContains(t, view, "drop")
}

func keyPress(key rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: key, Text: string(key)}
}

func TestContextDialogSelectionNavigation(t *testing.T) {
	t.Parallel()

	d := NewContextDialog(testBreakdownWithFiles()).(*contextDialog)
	d.SetSize(100, 50)
	d.View()

	require.Equal(t, 0, d.selected, "selection starts on the first attached file")

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 1, d.selected)

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // clamped at the last row
	assert.Equal(t, 2, d.selected)

	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // clamped at the first row
	assert.Equal(t, 0, d.selected)
}

func TestContextDialogDropSelected(t *testing.T) {
	t.Parallel()

	d := NewContextDialog(testBreakdownWithFiles()).(*contextDialog)
	d.SetSize(100, 50)
	d.View()

	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := d.Update(keyPress('d'))
	require.NotNil(t, cmd)

	msg := cmd()
	dropMsg, ok := msg.(messages.DropAttachedFileMsg)
	require.True(t, ok, "expected DropAttachedFileMsg, got %T", msg)
	assert.Equal(t, "/proj/docs/readme.md", dropMsg.Path)

	// The row is removed optimistically and the selection stays in bounds.
	require.Len(t, d.breakdown.AttachedFiles, 2)
	assert.Equal(t, "/proj/main.go", d.breakdown.AttachedFiles[0].Path)
	assert.Equal(t, "/tmp/deleted.txt", d.breakdown.AttachedFiles[1].Path)
	assert.Equal(t, 1, d.selected)
}

func TestContextDialogDropLastAttachment(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.AttachedFiles = []runtime.ContextFile{{Path: "/proj/only.go", Tokens: 10}}
	d := NewContextDialog(b).(*contextDialog)
	d.SetSize(100, 50)
	d.View()

	_, cmd := d.Update(keyPress('x'))
	require.NotNil(t, cmd)
	assert.Empty(t, d.breakdown.AttachedFiles)
	assert.Equal(t, -1, d.selected)

	// Further drop/navigation keys are inert and must not panic; up/down
	// fall back to scrolling.
	_, cmd = d.Update(keyPress('d'))
	assert.Nil(t, cmd)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, -1, d.selected)

	view := d.View()
	assert.NotContains(t, view, "Attached files")
}

func TestContextDialogPlainTextInventory(t *testing.T) {
	t.Parallel()

	d := &contextDialog{breakdown: testBreakdownWithFiles()}
	text := d.renderPlainText()

	assert.Contains(t, text, "Attached files")
	assert.Contains(t, text, "/proj/main.go")
	assert.Contains(t, text, "2.1K")
	assert.Contains(t, text, "/tmp/deleted.txt (missing)")
	assert.Contains(t, text, "Prompt files")
	assert.Contains(t, text, "/proj/AGENTS.md")
	assert.NotContains(t, text, "\x1b[", "plain text must carry no ANSI escapes")
}

func TestTruncateName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "short.go", truncateName("short.go", 24))
	assert.Equal(t, "a_very_long_file_name_w…", truncateName("a_very_long_file_name_with_suffix.go", 24))
	assert.LessOrEqual(t, lipgloss.Width(truncateName("日本語の長いファイル名.md", 10)), 10)
}

func TestFileLabelWidthClamped(t *testing.T) {
	t.Parallel()

	b := &runtime.ContextBreakdown{
		AttachedFiles: []runtime.ContextFile{{Path: "/p/a_very_long_file_name_that_exceeds_the_column_cap.go"}},
	}
	assert.Equal(t, 24, fileLabelWidth(b))

	b = &runtime.ContextBreakdown{AttachedFiles: []runtime.ContextFile{{Path: "/p/a.go"}}}
	assert.Equal(t, 4, fileLabelWidth(b))
}

// ---------------------------------------------------------------------------
// Live sessions (team view, targeted compaction)
// ---------------------------------------------------------------------------

func testLiveSessions() []runtime.LiveSession {
	return []runtime.LiveSession{
		{SessionID: "root-session-1234", AgentName: "root", InputTokens: 20_000, OutputTokens: 5_400, ContextLimit: 128_000, Current: true},
		// Two concurrent runs of the SAME agent: rows must stay distinct.
		{SessionID: "aaaa1111-e89b-12d3", AgentName: "developer", InputTokens: 50_000, OutputTokens: 5_100, ContextLimit: 200_000},
		{SessionID: "bbbb2222-e89b-12d3", AgentName: "developer", InputTokens: 1_000, OutputTokens: 200},
	}
}

func TestContextDialogLiveSessionsView(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdown(), testLiveSessions()...)
	dialog.SetSize(120, 50)
	view := dialog.View()

	assert.Contains(t, view, "Live sessions")
	assert.Contains(t, view, "root")
	assert.Contains(t, view, "(current)")
	assert.Contains(t, view, "developer")

	// Short session IDs disambiguate duplicate same-agent rows.
	assert.Contains(t, view, "aaaa1111")
	assert.Contains(t, view, "bbbb2222")

	// Budgets: used of limit with percentage, or the unknown-limit reading.
	assert.Contains(t, view, "55.1K of 200.0K (28%)")
	assert.Contains(t, view, "1.2K tokens, limit unknown")

	assert.Contains(t, view, "compact", "help must advertise the compact key")
}

// TestContextDialogLiveSessionsViewNoPerRowSuffix verifies live-session rows
// no longer render a per-row cap suffix (the /context header is the sole
// authority on the capped effective limit): a row whose CompactionModel is
// set renders exactly like an uncapped row.
func TestContextDialogLiveSessionsViewNoPerRowSuffix(t *testing.T) {
	t.Parallel()

	rows := testLiveSessions()
	rows[1].CompactionModel = "local/small"
	rows[1].PrimaryContextLimit = 200_000

	dialog := NewContextDialog(testBreakdown(), rows...)
	dialog.SetSize(120, 50)
	view := dialog.View()

	assert.NotContains(t, view, "capped by")
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, "aaaa1111") || strings.Contains(line, "bbbb2222") {
			assert.NotContains(t, line, "capped")
		}
	}
}

func TestContextDialogNoLiveSessions(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdown())
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.NotContains(t, view, "Live sessions")
	assert.NotContains(t, view, "compact")
}

func TestContextDialogEnterCompactsSelectedLiveSession(t *testing.T) {
	t.Parallel()

	d := NewContextDialog(testBreakdown(), testLiveSessions()...).(*contextDialog)
	d.SetSize(120, 50)
	d.View()

	require.Equal(t, 0, d.selected, "selection starts on the first live session")

	// Select the second developer row and request its compaction.
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	assert.IsType(t, CloseDialogMsg{}, msgs[0], "the dialog closes after requesting a compaction")
	compactMsg, ok := msgs[1].(messages.CompactSessionMsg)
	require.True(t, ok, "expected CompactSessionMsg, got %T", msgs[1])
	assert.Equal(t, "bbbb2222-e89b-12d3", compactMsg.SessionID)
	assert.Equal(t, "developer", compactMsg.AgentName)
	assert.Empty(t, compactMsg.AdditionalPrompt)
}

func TestContextDialogEnterOnMainRowTargetsRootSession(t *testing.T) {
	t.Parallel()

	d := NewContextDialog(testBreakdown(), testLiveSessions()...).(*contextDialog)
	d.SetSize(120, 50)
	d.View()

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	compactMsg, ok := msgs[1].(messages.CompactSessionMsg)
	require.True(t, ok, "expected CompactSessionMsg, got %T", msgs[1])
	assert.Equal(t, "root-session-1234", compactMsg.SessionID,
		"the main row carries the root session ID; the handler routes it through the classic /compact path")
}

func TestContextDialogCombinedSelection(t *testing.T) {
	t.Parallel()

	d := NewContextDialog(testBreakdownWithFiles(), testLiveSessions()...).(*contextDialog)
	d.SetSize(120, 50)
	d.View()

	// 3 live sessions + 3 attached files = 6 selectable rows.
	require.Equal(t, 6, d.selectableCount())

	// Walk from the live rows into the attached rows.
	for range 3 {
		d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	assert.Equal(t, 3, d.selected)
	idx, ok := d.selectedAttachedIndex()
	require.True(t, ok)
	assert.Equal(t, 0, idx)

	// Enter on an attached row is inert: no compaction, no close.
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)

	// Dropping works from the combined selection and targets the right file.
	_, cmd = d.Update(keyPress('d'))
	require.NotNil(t, cmd)
	dropMsg, ok := cmd().(messages.DropAttachedFileMsg)
	require.True(t, ok)
	assert.Equal(t, "/proj/main.go", dropMsg.Path)
	require.Len(t, d.breakdown.AttachedFiles, 2)

	// Selection clamps within the combined bounds after drops.
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, 4, d.selected)

	// The drop keys are inert while a live-session row is selected.
	for range 6 {
		d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	require.Equal(t, 0, d.selected)
	_, cmd = d.Update(keyPress('d'))
	assert.Nil(t, cmd)
	require.Len(t, d.breakdown.AttachedFiles, 2, "drop on a live row must not remove attachments")
}

func TestContextDialogEnterDoesNotClose(t *testing.T) {
	t.Parallel()

	// Without live sessions Enter is unbound: neither close nor compact.
	d := NewContextDialog(testBreakdown()).(*contextDialog)
	d.SetSize(100, 50)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)

	// Esc and q still close.
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	assert.IsType(t, CloseDialogMsg{}, cmd())
	_, cmd = d.Update(keyPress('q'))
	require.NotNil(t, cmd)
	assert.IsType(t, CloseDialogMsg{}, cmd())
}

func TestContextDialogPlainTextLiveSessions(t *testing.T) {
	t.Parallel()

	d := &contextDialog{breakdown: testBreakdown(), liveSessions: testLiveSessions()}
	text := d.renderPlainText()

	assert.Contains(t, text, "Live sessions")
	assert.Contains(t, text, "root  root-ses")
	assert.Contains(t, text, "(current)")
	assert.Contains(t, text, "developer  aaaa1111  55.1K of 200.0K (28%)")
	assert.Contains(t, text, "developer  bbbb2222  1.2K tokens, limit unknown")
	assert.NotContains(t, text, "\x1b[", "plain text must carry no ANSI escapes")
}

// TestContextDialogViewScalesWithCappedHeader is a regression test for the
// header block's variable height (an extra line renders when capped): the
// selection/scroll math derived from the rendered header must still land on
// the right row rather than an off-by-one from a stale line count.
func TestContextDialogViewScalesWithCappedHeader(t *testing.T) {
	t.Parallel()

	b := testBreakdownWithFiles()
	b.ContextLimit = 16_000
	b.CompactionModel = "local/small"
	b.CompactionContextLimit = 16_000
	b.PrimaryContextLimit = 128_000

	d := NewContextDialog(b, testLiveSessions()...).(*contextDialog)
	d.SetSize(120, 50)
	d.View()

	require.Equal(t, 0, d.selected, "selection still starts on the first live session despite the taller header")

	// Selection must still walk every row: 3 live sessions + 3 attached files.
	for range 5 {
		d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	assert.Equal(t, 5, d.selected)
}

// ---------------------------------------------------------------------------
// Latest compaction summary
// ---------------------------------------------------------------------------

func TestContextDialogViewCompactionSummaryText(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.LatestCompactionSummary = "We refactored the parser and fixed the tokenizer bug."

	dialog := NewContextDialog(b)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "Latest compaction summary")
	assert.Contains(t, view, "We refactored the parser")
}

// TestContextDialogCompactionSummaryWrapping pins the wrapping contract of
// the summary section at a deliberately small width: the text soft-wraps
// into more lines than its hard newlines alone produce, every line fits the
// width, hard newlines survive verbatim, and no content is lost. It fails
// if wrapTextLines stops wrapping (e.g. returns the text as one line).
func TestContextDialogCompactionSummaryWrapping(t *testing.T) {
	t.Parallel()

	const width = 24
	summary := "Goal: ship it.\n\nDecisions:\n" +
		strings.TrimSpace(strings.Repeat("keep the line math exact ", 4))

	b := testBreakdown()
	b.LatestCompactionSummary = summary
	d := &contextDialog{breakdown: b}

	lines := d.appendCompactionSummary(nil, width)
	require.Greater(t, len(lines), 2, "expected a spacer, the section title and the wrapped body")
	assert.Empty(t, lines[0])
	assert.Contains(t, lines[1], contextSummarySection)

	body := lines[2:]
	require.Greater(t, len(body), strings.Count(summary, "\n")+1,
		"the long paragraph must soft-wrap into more lines than hard newlines alone produce")

	trimmed := make([]string, len(body))
	for i, line := range body {
		assert.LessOrEqual(t, lipgloss.Width(line), width, "body line %d exceeds the wrap width", i)
		// Wrapped lines are padded to the full width; drop the padding so
		// the reassembly below only re-adds the spaces the wraps consumed.
		trimmed[i] = strings.TrimRight(line, " ")
	}

	// Hard newlines survive: the short first paragraph, the blank separator
	// and the "Decisions:" heading each keep their own line.
	assert.Equal(t, []string{"Goal: ship it.", "", "Decisions:"}, trimmed[:3])

	// No content is lost or reordered: undoing only the line breaks (each
	// soft wrap consumed one space) restores the original text.
	assert.Equal(t, strings.ReplaceAll(summary, "\n", " "), strings.Join(trimmed, " "))
}

func TestContextDialogViewNoCompactionSummaryText(t *testing.T) {
	t.Parallel()

	dialog := NewContextDialog(testBreakdown())
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.NotContains(t, view, "Latest compaction summary")
}

// TestContextDialogCompactionSummaryKeepsRowLines pins the layout contract
// behind #3870: the summary renders after the selectable sections, so with
// identical live sessions and files the rowLines mapping stays exactly the
// same with and without a long summary, covers every selectable row, and
// still scrolls off-screen rows into a low viewport.
func TestContextDialogCompactionSummaryKeepsRowLines(t *testing.T) {
	t.Parallel()

	render := func(summary string) *contextDialog {
		b := testBreakdownWithFiles()
		b.LatestCompactionSummary = summary
		d := NewContextDialog(b, testLiveSessions()...).(*contextDialog)
		d.SetSize(100, 16) // low window: the selectable rows start off screen
		d.View()
		return d
	}

	without := render("")
	with := render(strings.Repeat("a long compaction summary ", 20))

	require.Len(t, with.rowLines, with.selectableCount(),
		"every selectable row must be mapped to a content line")
	assert.Equal(t, without.rowLines, with.rowLines,
		"a summary appended after the selectable sections must not shift their content lines")

	// The mapping drives EnsureLineVisible: walking down to the last attached
	// file must scroll its (initially off-screen) row into the viewport.
	require.NotContains(t, with.View(), "deleted.txt", "the last attached row must start off screen")
	for range 5 {
		with.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	require.Equal(t, 5, with.selected)
	assert.Contains(t, with.View(), "deleted.txt",
		"navigating onto the row must bring it into view through rowLines")
}

func TestContextDialogPlainTextCompactionSummary(t *testing.T) {
	t.Parallel()

	b := testBreakdown()
	b.LatestCompactionSummary = "Line one of the summary.\nLine two, kept verbatim."

	d := &contextDialog{breakdown: b}
	text := d.renderPlainText()

	assert.Contains(t, text, "Latest compaction summary")
	assert.Contains(t, text, "Line one of the summary.\nLine two, kept verbatim.")
	assert.NotContains(t, text, "\x1b[", "plain text must carry no ANSI escapes")
}

func TestContextDialogPlainTextNoCompactionSummary(t *testing.T) {
	t.Parallel()

	d := &contextDialog{breakdown: testBreakdown()}
	assert.NotContains(t, d.renderPlainText(), "Latest compaction summary")
}
