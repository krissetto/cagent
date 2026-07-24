# Shared input/status exact-parity fixtures

Read-only reference source: `/home/krissetto/dev/agentic-tui`.
Reference composition paths: `pkg/tui/components/editor/editor.go`,
`pkg/tui/components/contextbar/contextbar.go`, `pkg/tui/components/card/card.go`,
`pkg/tui/shell/shell.go`, and `pkg/tui/tui_view_snapshot_test.go`.

`TestReferenceSharedInputStatusCellParity` fixes these inputs for both products:

- bundled default theme;
- editor focus;
- one active tab;
- context usage 0%;
- visible status bar;
- viewports 120x40 and 80x24;
- empty input and `first wrapped input line\nsecond input line`.

The shared region starts at the input separator and ends at the visible status
row. ANSI SGR is decoded to terminal cells and compared by row/column, glyph,
24-bit foreground/background, bold, italic, and underline. Every row is exactly
the viewport width; region heights are asserted before cell equality. This
makes an added spacer, shifted footer, changed padding/glyph/color/style, or
other geometry difference fail.

Declared normalization map (and only normalization):

- target status title `Docker Agent` -> reference status title `Agentic TUI`.

Product-specific menu/info/message/tab text is outside the extracted shared
region and is not normalized. Rows, whitespace, glyphs, ANSI, colors, styles,
and coordinates are never normalized.
