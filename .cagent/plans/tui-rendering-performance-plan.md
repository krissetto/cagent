# TUI Rendering Performance Improvement Plan

## Executive Summary

After an in-depth audit of the rendering architecture (`bubbletea` → `chatPage.View()` → `messages.View()` → `message.Render()` → `FastRenderer.Render()` → `scrollview.ViewWithLines()` → terminal), we identified **7 major inefficiency categories** causing excessive CPU usage during streaming and long-thread scenarios. The changes target **zero visual or behavioral changes** — only performance.

**Expected aggregate impact:** 3–10× reduction in CPU usage during active streaming with long threads.

---

## Table of Contents

1. [Architecture Overview & Rendering Path](#1-architecture-overview--rendering-path)
2. [Identified Bottlenecks (Ranked by Impact)](#2-identified-bottlenecks)
3. [Improvement Plan](#3-improvement-plan)
   - [P0: Incremental Markdown Rendering (Streaming Delta)](#p0-incremental-markdown-rendering)
   - [P1: Viewport-Only Rendering (Skip Off-Screen Messages)](#p1-viewport-only-rendering)
   - [P2: Cache Rendered Lines Per Message](#p2-cache-rendered-lines-per-message)
   - [P3: Eliminate Full Re-render on Animation Ticks](#p3-eliminate-full-re-render-on-animation-ticks)
   - [P4: Reduce lipgloss.Style.Render() Overhead in scrollview.compose()](#p4-reduce-lipgloss-overhead-in-scrollview-compose)
   - [P5: Reduce String Allocation in padAllLines and wrapText](#p5-reduce-string-allocation)
   - [P6: Optimize Selection Highlight (Hot Path on Mouse Drag)](#p6-optimize-selection-highlight)
4. [Benchmark Suite](#4-benchmark-suite)
5. [Implementation Order & Risk Assessment](#5-implementation-order--risk-assessment)
6. [Validation Checklist](#6-validation-checklist)

---

## 1. Architecture Overview & Rendering Path

### Component Hierarchy

```
tui.Model (bubbletea root)
  └─ chatPage (page/chat/chat.go)
       ├─ sidebar.Model
       └─ messages.Model (components/messages/messages.go)
            ├─ []message.Model (components/message/message.go)
            │     └─ markdown.FastRenderer (components/markdown/fast_renderer.go)
            ├─ []reasoningblock.Model
            ├─ []tool.Model
            └─ scrollview.Model (components/scrollview/scrollview.go)
                  └─ scrollbar.Model
```

### Rendering Flow (per frame)

```
1. bubbletea calls View() on root model
2. chatPage.View() calls p.messages.View()
3. messages.View():
   a. ensureAllItemsRendered()  ← EXPENSIVE: iterates ALL messages
      - For each message i:
        - renderItem(i, view) → view.View()
          - message.View() → FastRenderer.Render(fullContent)  ← RE-PARSES FULL MD
            - padAllLines(result, width)  ← SCANS FULL OUTPUT
        - lipgloss.Height(rendered)  ← COUNTS \n ACROSS FULL STRING
        - strings.Split(rendered, "\n")  ← ALLOCATES SLICE FOR ALL LINES
      - Appends all lines to m.renderedLines
   b. Slices visible window from m.renderedLines
   c. applySelectionHighlight (if active)
   d. scrollview.ViewWithLines(visibleLines)
      - compose(): pads each line, JoinHorizontal with scrollbar
4. chatPage wraps result in styles.ChatStyle.Render()
5. bubbletea diffs output against previous frame (line-by-line)
```

### The Streaming Problem

During streaming, `AgentChoiceEvent` arrives every ~50ms (throttled). Each event:
1. `AppendToLastMessage()` concatenates `content` to the last message's `.Content`
2. Calls `invalidateItem(lastIdx)` → sets `renderDirty = true`
3. Next `View()` call: `ensureAllItemsRendered()` re-renders **every single message**
   - Even though only the last message changed
   - The last message is re-parsed from scratch by `FastRenderer.Render()`
   - For a 2000-line markdown response, this means re-parsing 2000 lines ~20 times/second

### Key Metrics (Current State Estimates)

| Scenario | Messages | Last Msg Size | View() Cost per Frame |
|---|---|---|---|
| Short thread, short response | 5 | 20 lines | ~0.5ms |
| Medium thread, medium response | 30 | 200 lines | ~5ms |
| Long thread, long streaming response | 100+ | 1000+ lines | **~50-200ms** |
| Long thread with code blocks | 100+ | 500+ lines | **~100-500ms** |

At 14 FPS animation ticks (71ms budget), a 100ms+ View() causes visible frame drops and CPU spikes.

---

## 2. Identified Bottlenecks

### B1: Full Message List Re-render on Every Frame (CRITICAL)

**File:** `pkg/tui/components/messages/messages.go`, `ensureAllItemsRendered()` (line ~1072)

When `renderDirty` is true, **all** messages are re-rendered via `renderItem()`, even those not visible or unchanged. The cache (`m.renderedItems`) helps for completed messages, but during streaming:
- The streaming message is never cached (content is always changing)
- `invalidateItem(lastIdx)` deletes that item's cache entry
- But `renderDirty = true` still forces the entire list to be re-joined

**Cost:** O(totalMessages × averageMessageSize) per frame during streaming.

### B2: Full Markdown Re-parse on Every Content Append (CRITICAL)

**File:** `pkg/tui/components/markdown/fast_renderer.go`, `Render()` (line ~277)

Every time `AppendToLastMessage()` adds a few tokens, the next `View()` calls `FastRenderer.Render(entireMessageContent)`. For a 2000-line response being streamed, this means:
- Full `sanitizeForTerminal()` pass
- Full `parser.parse()` — line-by-line markdown parsing
- Full `wrapText()` for each paragraph/list item
- Full `padAllLines()` across entire output
- Full `syntaxHighlight()` for every code block (chroma tokenization)

**Cost:** O(messageContentLength) per frame, growing linearly as content streams in.

### B3: lipgloss.Height() Scans Entire Rendered String (MODERATE)

**File:** `pkg/tui/components/messages/messages.go`, `renderItem()` (line ~944)

```go
height := lipgloss.Height(rendered)
```

`lipgloss.Height()` counts newlines by scanning the full string. For a 2000-line rendered message, this is a redundant O(n) scan since we already split the output into lines.

### B4: strings.Split + strings.Join Churn (MODERATE)

**File:** `pkg/tui/components/messages/messages.go`, `ensureAllItemsRendered()` (line ~1092)

```go
viewContent := strings.TrimSuffix(item.view, "\n")
lines := strings.Split(viewContent, "\n")
allLines = append(allLines, lines...)
```

For every message, the rendered string is Split into lines, then all lines are appended to a flat array. For 100 messages averaging 20 lines each, that's 2000 individual string references being copied. Combined with the re-render on every frame during streaming, this is significant allocation pressure.

### B5: scrollview.compose() Per-Line Padding (MODERATE)

**File:** `pkg/tui/components/scrollview/scrollview.go`, `compose()` (line ~295)

```go
for i, line := range lines {
    w := ansi.StringWidth(line)
    // pad/truncate each line
}
contentView := strings.Join(lines, "\n")
```

Every visible line gets `ansi.StringWidth()` called (which must parse ANSI escapes), then potentially padded with `strings.Repeat(" ", ...)`. Then all lines are `strings.Join`'d. For 40-line viewport, this is 40 ansi-width calculations per frame.

Then `lipgloss.JoinHorizontal()` is called to merge content + scrollbar. This function internally splits both inputs by newline, measures widths, and joins — doing redundant work since we already know the widths.

### B6: Animation Tick Causes Full Invalidation When Animated Content Exists (MODERATE)

**File:** `pkg/tui/components/messages/messages.go`, `Update()` (line ~235)

```go
case animation.TickMsg:
    // Forward tick to all views that need it
    for i, view := range m.views { ... }
    if m.hasAnimatedContent() {
        m.invalidateItem(i) // for spinner/loading views
        m.renderDirty = true
    }
```

When any animated content exists (spinners, loading messages, running tools), every animation tick (14 FPS) sets `renderDirty = true`, causing a full `ensureAllItemsRendered()` on the next `View()`. This is unnecessary for non-animated messages.

### B7: padAllLines Redundant Width Scanning (LOW-MODERATE)

**File:** `pkg/tui/components/markdown/fast_renderer.go`, `padAllLines()` (line ~2111)

Called on the entire rendered output of every message. Scans every line for ANSI-aware width, then pads. For the streaming message's full output, this is O(totalOutputSize) per frame.

---

## 3. Improvement Plan

### P0: Incremental Markdown Rendering (Streaming Delta)

**Impact: ~5-10× improvement for streaming large messages**
**Risk: Medium** — Requires careful state management
**Files:** `fast_renderer.go`, `message.go`, `messages.go`

#### Problem
During streaming, the full message content is re-parsed from scratch on every frame. A 2000-line markdown document is fully parsed, styled, wrapped, and padded 14-20 times per second.

Additionally, in `message.Render()` we currently do:

```go
markdown.NewRenderer(width).Render(msg.Content)
```

`NewRenderer()` returns `NewFastRenderer(width)`, which allocates a new renderer object each call. This is small, but during streaming it happens every frame and becomes measurable.

#### Solution
Add incremental rendering support to `FastRenderer`. Instead of re-parsing the full content, identify the "stable prefix" of the markdown that hasn't changed and only re-render from the last paragraph boundary.

Also **reuse a renderer instance per message view** so un-cached messages (the last streaming message) avoid per-frame renderer allocation.

#### Design

**Approach A: Paragraph-boundary caching in `messageModel` + renderer reuse**

```go
// In message.go - messageModel
type messageModel struct {
    // ... existing fields ...

    // Reused renderer (avoid per-frame allocation)
    md *markdown.FastRenderer

    // Incremental rendering state
    lastRenderedContent string
    lastRenderedOutput  string
    stableOutput        string
}

func (mv *messageModel) SetSize(width, height int) tea.Cmd {
    mv.width = width
    mv.height = height

    // Rebuild renderer only when width changes
    contentWidth := width - styles.AssistantMessageStyle.GetHorizontalFrameSize()
    if mv.md == nil || mv.md.Width() != contentWidth {
        mv.md = markdown.NewFastRenderer(contentWidth)
    }
    return nil
}
```

If you don't want to make `FastRenderer` expose `.Width()`, store `mv.mdWidth int` alongside it.

**Incremental render algorithm:**
1. When `SetMessage()` gets new content:
   - If `strings.HasPrefix(new, old)` treat it as append
   - Find a stable block boundary near the end of `old`
   - Re-render only the suffix from that boundary
2. For non-append changes: full render

**Approach B: Renderer reuse at the messages component level**

If you prefer not to embed renderer in `messageModel`, `messages.model` can maintain a `sync.Pool` keyed by width and hand out renderers to message views while rendering. This is more invasive and harder to keep stable with selection + caching.

#### Validation
- Render full content and compare with incremental result
- Enable comparison mode in debug builds to catch divergences
- Benchmark: `BenchmarkIncrementalRender` (see section 4)

---

### P1: Viewport-Only Rendering (Skip Off-Screen Messages)

**Impact: ~2-5× improvement for long threads**
**Risk: Low** — Cache already exists, just needs viewport awareness
**Files:** `messages.go`

#### Problem
`ensureAllItemsRendered()` iterates ALL messages and calls `renderItem()` + `strings.Split()` for each one, even if only 3-4 are visible in the viewport. With 100+ messages, most rendering work is wasted.

#### Solution
Replace the flat `ensureAllItemsRendered()` with a two-pass approach:
1. **Height estimation pass** (cheap): For each message, get or estimate its rendered height without producing the full rendered string
2. **Render only visible messages** (on-demand): Only call `view.View()` for messages that overlap the viewport

#### Design

```go
type model struct {
    // ... existing fields ...

    // Per-message cached data
    itemHeights    []int              // Cached height per message (0 = unknown)
    itemLines      [][]string         // Cached split lines per message
    itemDirty      []bool             // Per-item dirty flag (replaces global renderDirty for items)
}
```

**Height estimation:**
- For cached messages: use stored height from `renderedItems[i].height`
- For uncached messages that haven't been rendered: estimate from content length
  - `estimatedLines = max(1, strings.Count(msg.Content, "\n") + 1)`
  - This is a rough estimate but is only used for scroll offset calculations
- When a message is first rendered, store its actual height

**ensureAllItemsRendered() replacement:**

```go
func (m *model) ensureVisibleRendered() {
    // Phase 1: Calculate cumulative heights to find which messages are visible
    cumulativeHeight := 0
    firstVisible, lastVisible := -1, -1
    
    for i := range m.messages {
        h := m.getItemHeight(i)  // cached or estimated
        if cumulativeHeight + h > m.scrollOffset && firstVisible == -1 {
            firstVisible = i
        }
        if cumulativeHeight < m.scrollOffset + m.height {
            lastVisible = i
        }
        cumulativeHeight += h
        if m.needsSeparator(i) {
            cumulativeHeight++
        }
    }
    
    // Phase 2: Only render visible messages (with 1-message buffer above/below)
    firstVisible = max(0, firstVisible - 1)
    lastVisible = min(len(m.messages) - 1, lastVisible + 1)
    
    // Phase 3: Build visible lines slice
    // ... only from firstVisible to lastVisible
}
```

**Critical detail:** The total height must still be tracked accurately for scrollbar sizing. For messages that have been rendered at least once, use the cached height. For never-rendered messages, use the estimate.

#### Compatibility
- `scrollToSelectedMessage()` calls `ensureAllItemsRendered()` — replace with targeted rendering
- `applySelectionHighlight()` operates on `m.renderedLines` — adjust to work with the sparse visible window

---

### P2: Cache Rendered Lines Per Message

**Impact: ~2× improvement across all scenarios**
**Risk: Low**
**Files:** `messages.go`

#### Problem
Currently, `renderedItems` caches the full rendered string and its height. But `ensureAllItemsRendered()` then does `strings.Split(item.view, "\n")` to get lines, creating a new slice every time. The `renderedLines` flat array is rebuilt from scratch whenever `renderDirty` is true.

#### Solution
Cache the split lines alongside the rendered string:

```go
type renderedItem struct {
    view   string
    lines  []string  // Pre-split lines (avoids repeated Split)
    height int
}
```

When `renderItem()` produces output, pre-split:
```go
item.lines = strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
item.height = len(item.lines)
```

Then `ensureAllItemsRendered()` becomes:
```go
for i, view := range m.views {
    item := m.renderItem(i, view)
    allLines = append(allLines, item.lines...)
    if m.needsSeparator(i) {
        allLines = append(allLines, "")
    }
}
```

This eliminates:
- Redundant `strings.TrimSuffix` per message
- Redundant `strings.Split` per message  
- The `lipgloss.Height()` call (replaced by `len(item.lines)`)

---

### P3: Eliminate Full Re-render on Animation Ticks

**Impact: ~2× improvement when spinners are active during streaming**
**Risk: Low**
**Files:** `messages.go`

#### Problem
In `Update()` for `animation.TickMsg`:
```go
if m.hasAnimatedContent() {
    // ticks forwarded to spinner views
    m.invalidateItem(i)
}
```

This sets `renderDirty = true`, causing `ensureAllItemsRendered()` to re-render ALL messages on every tick, even though only the spinner/animated views changed.

#### Solution
Use per-item dirty tracking instead of global `renderDirty`:

```go
case animation.TickMsg:
    for i, view := range m.views {
        if m.isAnimated(i) {
            updatedView, cmd := view.Update(msg)
            m.views[i] = updatedView
            m.itemDirty[i] = true  // Only mark THIS item dirty
            cmds = append(cmds, cmd)
        }
    }
    // Don't set global renderDirty — only rebuild affected lines
```

Then in `ensureAllItemsRendered()` (or its replacement):
- Only re-render items where `itemDirty[i]` is true
- Splice new lines into `renderedLines` at the correct offset
- Adjust cumulative heights

**Splice approach:**
```go
func (m *model) updateDirtyItems() {
    offset := 0
    for i := range m.messages {
        oldHeight := m.itemHeights[i]
        if m.itemDirty[i] {
            item := m.renderItem(i, m.views[i])
            newLines := item.lines
            // Replace old lines at offset..offset+oldHeight with newLines
            m.renderedLines = slices.Replace(m.renderedLines, offset, offset+oldHeight, newLines...)
            m.itemHeights[i] = len(newLines)
            m.itemDirty[i] = false
        }
        offset += m.itemHeights[i]
        if m.needsSeparator(i) {
            offset++
        }
    }
    m.totalHeight = offset
}
```

---

### P4: Reduce lipgloss Overhead in scrollview.compose()

**Impact: ~1.5× improvement for render phase**
**Risk: Low**
**Files:** `scrollview.go`, `messages.go`

#### Problem
`compose()` does three expensive operations per frame:
1. `ansi.StringWidth(line)` for each visible line (40 calls at typical viewport height)
2. `strings.Repeat(" ", padding)` for padding
3. `lipgloss.JoinHorizontal()` to merge content with scrollbar — internally re-splits by newline, measures, and joins

#### Solution

**4a: Pre-pad lines in the markdown renderer**

The `FastRenderer.padAllLines()` already pads to the content width. If we ensure the width matches `scrollview.ContentWidth()`, the compose step can skip re-measuring.

Pass `contentWidth` from messages component down to the renderer:
```go
// In message.Render():
rendered, err := markdown.NewRenderer(contentWidth).Render(msg.Content)
// contentWidth already accounts for scrollbar reservation
```

Then in `compose()`, skip the width-check loop for pre-padded content:
```go
func (m *Model) compose(lines []string) string {
    // Lines are already padded to contentWidth — just join
    contentView := strings.Join(lines, "\n")
    
    if m.NeedsScrollbar() {
        // Build scrollbar column manually instead of JoinHorizontal
        return m.composeWithScrollbar(contentView, lines)
    }
    return contentView
}
```

**4b: Replace lipgloss.JoinHorizontal with direct string building**

```go
func (m *Model) composeWithScrollbar(contentView string, lines []string) string {
    sbLines := m.sb.ViewLines()  // Get scrollbar as []string
    gap := strings.Repeat(" ", m.gapWidth)
    
    var b strings.Builder
    b.Grow(len(contentView) + len(lines) * (m.gapWidth + scrollbar.Width + 1))
    
    for i, line := range lines {
        if i > 0 {
            b.WriteByte('\n')
        }
        b.WriteString(line)
        b.WriteString(gap)
        if i < len(sbLines) {
            b.WriteString(sbLines[i])
        } else {
            b.WriteByte(' ')
        }
    }
    return b.String()
}
```

This eliminates `lipgloss.JoinHorizontal()` overhead entirely.

**Note:** The scrollbar component would need a `ViewLines() []string` method. If modifying the scrollbar is undesirable, render the scrollbar view once and split it:
```go
sbView := m.sb.View()
sbLines := strings.Split(sbView, "\n")
```

---

### P5: Reduce String Allocation in padAllLines and wrapText

**Impact: ~1.3× for large messages**
**Risk: Low**
**Files:** `fast_renderer.go`

#### Problem

**padAllLines:** Scans every line in the entire rendered output for ANSI-aware width, then pads. This is called on the FULL rendered output of each message, not just the delta.

**wrapText:** Called per paragraph/list-item. Uses `splitWordsWithStyles()` which allocates a `[]styledWord` slice, and each wrap produces a new string via `strings.Builder`.

#### Solution

**5a: Integrate padding into the renderer itself**

Instead of a final `padAllLines()` pass, pad each line as it's written to `p.out`:

```go
// Add a helper to the parser
func (p *parser) writePaddedLine(line string) {
    p.out.WriteString(line)
    w := ansiStringWidth(line)
    if w < p.width {
        writeSpaces(&p.out, p.width - w)
    }
    p.out.WriteByte('\n')
}
```

Use this throughout the renderer instead of bare `p.out.WriteString(wrapped + "\n")`. This eliminates the final `padAllLines()` pass entirely.

**Caveat:** Multi-line wrapped output from `wrapText()` would need each line padded individually. Modify `wrapText()` to accept the builder directly and pad inline.

**5b: Pool styledWord slices in wrapText**

```go
var styledWordPool = sync.Pool{
    New: func() any {
        s := make([]styledWord, 0, 64)
        return &s
    },
}
```

**5c: Pre-size the parser's output buffer more accurately**

```go
// In parser.reset():
p.out.Grow(len(input) * 3)  // Styled output is typically 2-3× raw input size
```

---

### P6: Optimize Selection Highlight (Hot Path on Mouse Drag)

**Impact: ~1.5× during active text selection**
**Risk: Low**
**Files:** `selection.go`

#### Problem
`applySelectionHighlight()` is called on every frame when selection is active. For each visible line in the selection range:
1. `ansi.Strip(line)` — allocates stripped string
2. `runewidth.StringWidth()` — scans for width
3. `ansi.Cut(line, ...)` — three calls, each doing ANSI-aware scanning
4. `styles.SelectionStyle.Render(selectedPlain)` — lipgloss render overhead

During mouse drag selection, this runs at mouse-motion rate (potentially 60+ Hz).

#### Solution

**6a: Pre-compute the selection style as raw ANSI**

```go
var selectionPrefix, selectionSuffix string

func init() {
    const marker = "\x00"
    rendered := styles.SelectionStyle.Render(marker)
    selectionPrefix, selectionSuffix, _ = strings.Cut(rendered, marker)
}
```

Then in `highlightLine()`:
```go
selected := selectionPrefix + selectedPlain + selectionSuffix
```

This avoids `lipgloss.Style.Render()` per highlighted line.

**6b: Combine the strip + width + cut into a single pass**

Write a custom `highlightLineRange(line string, startCol, endCol int) string` that walks the string once, tracking ANSI state and display width, and emits the three segments (before, highlighted, after) in one pass.

---

## 4. Benchmark Suite

All benchmark code should be placed in the corresponding `_test.go` files. Run with:

```bash
# Full benchmark suite
go test ./pkg/tui/components/markdown/ -bench=. -benchmem -count=5
go test ./pkg/tui/components/messages/ -bench=. -benchmem -count=5

# Compare before/after
go test ./pkg/tui/components/markdown/ -bench=. -benchmem -count=10 > old.txt
# (apply changes)
go test ./pkg/tui/components/markdown/ -bench=. -benchmem -count=10 > new.txt
benchstat old.txt new.txt
```

### Benchmark File: `pkg/tui/components/markdown/bench_perf_test.go`

```go
package markdown

import (
	"fmt"
	"strings"
	"testing"
)

// generateLargeMarkdown creates realistic streaming-like content at various sizes.
func generateLargeMarkdown(paragraphs, codeBlocks int) string {
	var b strings.Builder
	for i := range paragraphs {
		b.WriteString(fmt.Sprintf("## Section %d\n\n", i+1))
		b.WriteString("This is a paragraph with **bold text**, *italic text*, and `inline code`. ")
		b.WriteString("It contains [links](https://example.com) and various markdown elements. ")
		b.WriteString("The quick brown fox jumps over the lazy dog. Lorem ipsum dolor sit amet, ")
		b.WriteString("consectetur adipiscing elit. Sed do eiusmod tempor incididunt ut labore.\n\n")
		if i < codeBlocks {
			b.WriteString("```go\n")
			b.WriteString("package main\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n")
			b.WriteString("func main() {\n")
			b.WriteString("\tresult := strings.Builder{}\n")
			b.WriteString("\tfor i := 0; i < 100; i++ {\n")
			b.WriteString("\t\tresult.WriteString(fmt.Sprintf(\"line %d\\n\", i))\n")
			b.WriteString("\t}\n")
			b.WriteString("\tfmt.Println(result.String())\n")
			b.WriteString("}\n")
			b.WriteString("```\n\n")
		}
		b.WriteString("- List item one with **bold**\n")
		b.WriteString("- List item two with *italic*\n")
		b.WriteString("- List item three with `code`\n\n")
	}
	return b.String()
}

// BenchmarkFastRendererSmall benchmarks rendering a small message (typical user msg).
func BenchmarkFastRendererSmall(b *testing.B) {
	content := "This is a short message with **bold** and `code`."
	r := NewFastRenderer(80)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Render(content)
	}
}

// BenchmarkFastRendererMedium benchmarks a medium assistant response (~50 lines).
func BenchmarkFastRendererMedium(b *testing.B) {
	content := generateLargeMarkdown(5, 2)
	r := NewFastRenderer(100)
	b.ReportMetric(float64(len(content)), "inputBytes")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Render(content)
	}
}

// BenchmarkFastRendererLarge benchmarks a large assistant response (~500 lines).
func BenchmarkFastRendererLarge(b *testing.B) {
	content := generateLargeMarkdown(30, 10)
	r := NewFastRenderer(100)
	b.ReportMetric(float64(len(content)), "inputBytes")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Render(content)
	}
}

// BenchmarkFastRendererHuge benchmarks an extremely large response (~2000 lines).
func BenchmarkFastRendererHuge(b *testing.B) {
	content := generateLargeMarkdown(100, 30)
	r := NewFastRenderer(100)
	b.ReportMetric(float64(len(content)), "inputBytes")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Render(content)
	}
}

// BenchmarkFastRendererStreaming simulates streaming: render progressively larger content.
// This is the most important benchmark — it simulates what happens during actual streaming.
func BenchmarkFastRendererStreaming(b *testing.B) {
	fullContent := generateLargeMarkdown(30, 10)
	r := NewFastRenderer(100)

	// Simulate streaming chunks of ~100 characters
	chunks := make([]string, 0, len(fullContent)/100+1)
	for i := 0; i < len(fullContent); i += 100 {
		end := min(i+100, len(fullContent))
		chunks = append(chunks, fullContent[:end])
	}

	b.ReportMetric(float64(len(chunks)), "chunks")
	b.ReportMetric(float64(len(fullContent)), "finalBytes")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for _, chunk := range chunks {
			_, _ = r.Render(chunk)
		}
	}
}

// BenchmarkPadAllLines benchmarks the padAllLines function directly.
func BenchmarkPadAllLines(b *testing.B) {
	content := generateLargeMarkdown(30, 10)
	r := NewFastRenderer(100)
	rendered, _ := r.Render(content)
	width := 100

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = padAllLines(rendered, width)
	}
}

// BenchmarkWrapText benchmarks text wrapping on a long paragraph.
func BenchmarkWrapText(b *testing.B) {
	p := &parser{}
	p.styles = getGlobalStyles()
	longParagraph := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50)
	rendered := p.renderInline(longParagraph)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.wrapText(rendered, 80)
	}
}

// BenchmarkIncrementalVsFullRender compares full re-render vs what incremental would save.
// After P0 is implemented, add the incremental benchmark here.
func BenchmarkIncrementalVsFullRender(b *testing.B) {
	base := generateLargeMarkdown(20, 8)
	delta := "\n\nAnd here is one more paragraph of **additional** content with `code`.\n"
	full := base + delta
	r := NewFastRenderer(100)

	b.Run("full_re-render", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = r.Render(full)
		}
	})

	// After implementing P0, add:
	// b.Run("incremental", func(b *testing.B) {
	//     r.Render(base)  // prime the cache
	//     b.ResetTimer()
	//     for i := 0; i < b.N; i++ {
	//         _, _ = r.RenderIncremental(base, full)
	//     }
	// })
}
```

### Benchmark File: `pkg/tui/components/messages/bench_perf_test.go`

```go
package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/cagent/pkg/tui/service"
	"github.com/docker/cagent/pkg/tui/types"
)

func generateMarkdownContent(sections int) string {
	var b strings.Builder
	for i := range sections {
		b.WriteString(fmt.Sprintf("## Section %d\n\n", i+1))
		b.WriteString("This is a paragraph with **bold** and `code`. Lorem ipsum dolor sit amet.\n\n")
		if i%3 == 0 {
			b.WriteString("```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n\n")
		}
		b.WriteString("- Item one\n- Item two\n- Item three\n\n")
	}
	return b.String()
}

// setupMessagesModel creates a messages model with n messages for benchmarking.
func setupMessagesModel(n int, contentSections int) *model {
	sessionState := service.NewSessionState()
	m := New(sessionState).(*model)
	m.SetSize(120, 40)

	for i := range n {
		if i%2 == 0 {
			msg := types.User(fmt.Sprintf("User message %d", i))
			m.messages = append(m.messages, msg)
			view := m.createMessageView(msg)
			m.views = append(m.views, view)
		} else {
			msg := types.Agent(types.MessageTypeAssistant, "assistant", generateMarkdownContent(contentSections))
			m.messages = append(m.messages, msg)
			view := m.createMessageView(msg)
			m.views = append(m.views, view)
		}
	}
	m.renderDirty = true
	return m
}

// BenchmarkMessagesViewSmallThread benchmarks View() with a small thread.
func BenchmarkMessagesViewSmallThread(b *testing.B) {
	m := setupMessagesModel(10, 3)
	m.View() // Prime
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderDirty = true
		m.renderedLines = nil
		m.renderedItems = make(map[int]renderedItem)
		_ = m.View()
	}
}

// BenchmarkMessagesViewLargeThread benchmarks View() with a large thread (100 messages).
func BenchmarkMessagesViewLargeThread(b *testing.B) {
	m := setupMessagesModel(100, 5)
	m.View() // Prime
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderDirty = true
		m.renderedLines = nil
		m.renderedItems = make(map[int]renderedItem)
		_ = m.View()
	}
}

// BenchmarkMessagesViewCached benchmarks View() when cache is warm (no changes).
func BenchmarkMessagesViewCached(b *testing.B) {
	m := setupMessagesModel(100, 5)
	m.View() // Prime cache
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkMessagesViewStreaming simulates streaming: append to last message and re-render.
func BenchmarkMessagesViewStreaming(b *testing.B) {
	m := setupMessagesModel(50, 3)
	// Add a streaming assistant message
	msg := types.Agent(types.MessageTypeAssistant, "assistant", "")
	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	m.scrollToBottom()

	chunk := "Here is some **streaming** content with `code` and more text. "
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Simulate streaming append
		lastIdx := len(m.messages) - 1
		m.messages[lastIdx].Content += chunk
		m.invalidateItem(lastIdx)
		_ = m.View()
	}
}

// BenchmarkMessagesViewStreamingLongMessage simulates streaming a long message.
func BenchmarkMessagesViewStreamingLongMessage(b *testing.B) {
	m := setupMessagesModel(30, 3)
	// Pre-fill a large message
	largeContent := generateMarkdownContent(20)
	msg := types.Agent(types.MessageTypeAssistant, "assistant", largeContent)
	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	m.scrollToBottom()

	chunk := "More content. "
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		lastIdx := len(m.messages) - 1
		m.messages[lastIdx].Content += chunk
		m.invalidateItem(lastIdx)
		_ = m.View()
	}
}

// BenchmarkEnsureAllItemsRendered benchmarks the core rendering loop.
func BenchmarkEnsureAllItemsRendered(b *testing.B) {
	m := setupMessagesModel(100, 5)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.renderDirty = true
		m.renderedLines = nil
		m.renderedItems = make(map[int]renderedItem)
		m.ensureAllItemsRendered()
	}
}
```

### Benchmark File: `pkg/tui/components/scrollview/bench_perf_test.go`

```go
package scrollview

import (
	"fmt"
	"strings"
	"testing"
)

func generateStyledLines(n, width int) []string {
	lines := make([]string, n)
	for i := range n {
		// Simulate ANSI-styled content of varying widths
		content := fmt.Sprintf("\x1b[1m\x1b[34mLine %d:\x1b[0m Some content here with styling", i)
		if len(content) < width {
			content += strings.Repeat(" ", width-len(content)) // rough padding
		}
		lines[i] = content
	}
	return lines
}

// BenchmarkComposeWithScrollbar benchmarks the compose function with scrollbar.
func BenchmarkComposeWithScrollbar(b *testing.B) {
	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(120, 40)
	lines := generateStyledLines(100, 116)
	m.SetContent(lines, 100)
	m.SetScrollOffset(0)

	visible := lines[:40]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.ViewWithLines(visible)
	}
}

// BenchmarkComposeNoScrollbar benchmarks compose without scrollbar.
func BenchmarkComposeNoScrollbar(b *testing.B) {
	m := New()
	m.SetSize(120, 40)
	lines := generateStyledLines(30, 120)
	m.SetContent(lines, 30)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.ViewWithLines(lines)
	}
}
```

---

## 5. Implementation Order & Risk Assessment

| Priority | Change | Expected Gain | Risk | Dependencies | Effort |
|----------|--------|---------------|------|--------------|--------|
| **P0** | Incremental markdown rendering | 5-10× streaming | Medium | None | 3-4 days |
| **P1** | Viewport-only rendering | 2-5× long threads | Low | P2 (recommended) | 2-3 days |
| **P2** | Cache rendered lines per message | 2× all scenarios | Low | None | 0.5 day |
| **P3** | Per-item dirty tracking | 2× with animations | Low | P2 | 1 day |
| **P4** | Eliminate lipgloss in compose | 1.5× render phase | Low | None | 1 day |
| **P5** | Inline padding, pool allocations | 1.3× large messages | Low | None | 1 day |
| **P6** | Optimize selection highlight | 1.5× during selection | Low | None | 0.5 day |

### Recommended Implementation Order

1. **P2 first** — Simplest change, enables P1 and P3, immediate benefit
2. **P3** — Quick win once P2 is in place
3. **P0** — Biggest single improvement, but most complex
4. **P1** — Major improvement for long threads
5. **P4** — Clean improvement to compose path
6. **P5** — Micro-optimizations in renderer
7. **P6** — Selection-specific

### Risk Mitigation

- **P0 (Medium risk):** Implement with a `DEBUG_RENDER_VERIFY` env var that does both full and incremental renders and compares output. Run the existing `fast_renderer_test.go` tests after every change.
- **P1 (Low risk):** Keep `ensureAllItemsRendered()` as a fallback. New code path only activates when `len(m.messages) > viewportMessages + 2`.
- **All changes:** Run the full test suite (`go test ./pkg/tui/...`) and the benchmark suite before/after.

---

## 6. Validation Checklist

### Before Starting
- [ ] Run existing benchmarks and record baseline numbers
- [ ] Run `go test ./pkg/tui/...` and confirm all tests pass
- [ ] Profile with `go tool pprof` during a streaming session to establish CPU baseline
  ```bash
  # Add temporary profiling to the binary
  import "runtime/pprof"
  f, _ := os.Create("cpu.prof")
  pprof.StartCPUProfile(f)
  defer pprof.StopCPUProfile()
  ```

### After Each Change
- [ ] Run benchmark suite, compare with `benchstat`
- [ ] Run `go test ./pkg/tui/...`
- [ ] Manual smoke test:
  - Start a session, send a message that produces a long response (~500 lines)
  - Verify output looks identical to before
  - Verify scrolling works correctly during streaming
  - Verify scrollbar position is correct
  - Verify text selection works during streaming
  - Verify animation ticks (spinners) don't cause visible lag
  - Verify message selection (keyboard nav) works correctly
  - Verify inline editing works
  - Test with terminal resize during streaming

### After All Changes
- [ ] CPU profile comparison: before vs after during streaming
- [ ] Memory profile: no leaks from caching
- [ ] Long-running test: leave streaming for 5+ minutes, check memory doesn't grow unbounded
- [ ] Test with narrow terminal (< 40 columns)
- [ ] Test with very wide terminal (> 200 columns)
- [ ] Test with sidebar visible and hidden
- [ ] Test session restore with large history

---

## Appendix: Notes on Architecture Decisions

### Why NOT replace Bubble Tea?

Bubble Tea's core loop is not the bottleneck. The Elm architecture (Update/View) with bubbletea's line-diffing renderer is actually well-suited for TUI rendering. The bottleneck is entirely in **our View() implementation** doing too much work per frame. Bubble Tea's diff-based output (only rewriting changed lines to the terminal) is a net positive.

Replacing Bubble Tea would be a massive effort with no proportional performance gain, since the terminal I/O layer is not the issue.

### Why NOT use glamour?

The `FastRenderer` already replaced glamour and is significantly faster. glamour builds a full goldmark AST, walks it, and renders through multiple layers. The `FastRenderer`'s single-pass approach is the right architecture — it just needs incremental support.

### Why keep the flat renderedLines array?

The scrollview's line-slicing approach (viewport window into a flat array) is actually efficient for the final compose step. The issue is how that array gets populated. Once we have viewport-only rendering (P1), the array only contains visible lines plus a small buffer, making all operations O(viewportHeight) rather than O(totalMessages).

### Thread safety notes

The rendering path is single-threaded (Bubble Tea's Update/View loop). All caching is safe without additional synchronization. The animation coordinator already uses a mutex for its registration bookkeeping, which is correct.
