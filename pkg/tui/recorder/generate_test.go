package recorder

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyAt(at time.Time, frame string, code rune, text string) event {
	return event{kind: keyEvent, key: tea.KeyPressMsg{Code: code, Text: text}, at: at, frame: frame}
}

func typing(at time.Time, frame, text string) []event {
	events := make([]event, 0, len(text))
	for i, r := range text {
		events = append(events, keyAt(at.Add(time.Duration(i)*50*time.Millisecond), frame, r, string(r)))
	}
	return events
}

// parseValid asserts src is valid Go and returns it for further inspection.
func parseValid(t *testing.T, src string) string {
	t.Helper()
	_, err := parser.ParseFile(token.NewFileSet(), "generated_test.go", src, parser.ParseComments)
	require.NoError(t, err, "generated test must be valid Go source:\n%s", src)
	return src
}

// TestGenerateTest_BasicChat replays the canonical record-mode session: the
// user types a question, submits it, reads the streamed answer, and quits
// with Ctrl+C. The generated test must type the question, wait for the
// answer, and drop the quit keystroke.
func TestGenerateTest_BasicChat(t *testing.T) {
	base := time.Now()
	const emptyFrame = "│ Ask anything\n─ status bar ─"
	const answerFrame = "What's 2+2?\n2 + 2 equals 4.\n─ status bar ─"

	r := &Recorder{inner: &fakeModel{}, width: 120, height: 40}
	r.events = append(r.events, typing(base, emptyFrame, "What's 2+2?")...)
	r.events = append(r.events,
		keyAt(base.Add(time.Second), emptyFrame, tea.KeyEnter, ""),
		keyAt(base.Add(8*time.Second), answerFrame, 'c', ""),
	)
	r.events[len(r.events)-1].key.Mod = tea.ModCtrl

	src := parseValid(t, r.GenerateTest(GenerateOptions{
		AgentFile:    "./basic.yaml",
		CassettePath: "my-scenario.yaml",
	}))

	assert.Contains(t, src, "func TestRecordedMyScenario(t *testing.T)")
	assert.Contains(t, src, `newTUI(t, "testdata/basic.yaml", 120, 40)`)
	assert.Contains(t, src, `WaitFor(tuitest.Not(tuitest.Contains("Loading")))`)
	assert.Contains(t, src, `Type("What's 2+2?")`)
	assert.Contains(t, src, "Enter()")
	// The final wait anchors on the longest new line: the answer, not the echo.
	assert.Contains(t, src, `WaitFor(tuitest.Contains("2 + 2 equals 4."))`)
	// The quitting Ctrl+C must not be replayed.
	assert.NotContains(t, src, "Press")
	// No special keys used, so tea must not be imported.
	assert.NotContains(t, src, `charm.land/bubbletea`)
}

// TestGenerateTest_WaitBetweenPrompts pins that an idle pause with new screen
// content between two prompts becomes a WaitFor separating the Type calls.
func TestGenerateTest_WaitBetweenPrompts(t *testing.T) {
	base := time.Now()
	const frame1 = "│ Ask anything"
	const frame2 = "hi\nHello! How can I help you today?"

	r := &Recorder{inner: &fakeModel{}}
	r.events = append(r.events, typing(base, frame1, "hi")...)
	r.events = append(r.events, keyAt(base.Add(time.Second), frame1, tea.KeyEnter, ""))
	r.events = append(r.events, typing(base.Add(10*time.Second), frame2, "bye")...)

	src := parseValid(t, r.GenerateTest(GenerateOptions{
		AgentFile:    "a.yaml",
		CassettePath: "two-prompts.yaml",
	}))

	first := strings.Index(src, `Type("hi")`)
	wait := strings.Index(src, `WaitFor(tuitest.Contains("Hello! How can I help you today?"))`)
	second := strings.Index(src, `Type("bye")`)
	require.True(t, first >= 0 && wait >= 0 && second >= 0, "missing steps in:\n%s", src)
	assert.Less(t, first, wait)
	assert.Less(t, wait, second)
}

// TestGenerateTest_SpecialKeysAndClicks covers Press mapping (with modifier),
// ClickText synthesis, and the resulting tea import.
func TestGenerateTest_SpecialKeysAndClicks(t *testing.T) {
	base := time.Now()
	const frame = "line one\n  Copy  answer text"

	r := &Recorder{inner: &fakeModel{}}
	r.events = append(r.events,
		keyAt(base, frame, 'k', ""),
		keyAt(base.Add(100*time.Millisecond), frame, tea.KeyEscape, ""),
		event{kind: clickEvent, x: 3, y: 1, at: base.Add(200 * time.Millisecond), frame: frame},
	)
	r.events[0].key.Mod = tea.ModCtrl

	src := parseValid(t, r.GenerateTest(GenerateOptions{
		AgentFile:    "a.yaml",
		CassettePath: "keys.yaml",
	}))

	assert.Contains(t, src, `Press('k', tea.ModCtrl)`)
	assert.Contains(t, src, "Press(tea.KeyEscape)")
	assert.Contains(t, src, `ClickText("Copy")`)
	assert.Contains(t, src, `tea "charm.land/bubbletea/v2"`)
	// Recorder never saw a WindowSizeMsg: dimensions fall back to defaults.
	assert.Contains(t, src, `120, 40`)
}

func TestTrimTrailingQuit(t *testing.T) {
	base := time.Now()
	ctrlC := keyAt(base, "settled frame", 'c', "")
	ctrlC.key.Mod = tea.ModCtrl

	kept, quitFrame, trimmed := trimTrailingQuit([]event{keyAt(base, "f", 'a', "a"), ctrlC, ctrlC})

	assert.Len(t, kept, 1)
	assert.True(t, trimmed)
	assert.Equal(t, "settled frame", quitFrame, "quit frame must be what the user saw when quitting")

	// A Ctrl+C in the middle of the session is kept.
	kept, _, trimmed = trimTrailingQuit([]event{ctrlC, keyAt(base, "f", 'a', "a")})
	assert.Len(t, kept, 2)
	assert.False(t, trimmed)
}

// TestIsQuitKey pins the Bubble Tea v2 representation of Ctrl+C/Ctrl+D: the
// legacy input path maps the raw ETX/EOT bytes to {Mod: ModCtrl, Code:
// letter} (ultraviolet's key table), and the Kitty path adds lock modifiers
// and BaseCode.
func TestIsQuitKey(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyPressMsg
		want bool
	}{
		{"legacy ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, true},
		{"legacy ctrl+d", tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, true},
		{"kitty ctrl+c with caps lock", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModCapsLock}, true},
		{"kitty ctrl+c on non-latin layout", tea.KeyPressMsg{Code: 'с', BaseCode: 'c', Mod: tea.ModCtrl}, true},
		{"plain c", tea.KeyPressMsg{Code: 'c', Text: "c"}, false},
		{"ctrl+shift+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift}, false},
		{"ctrl+q", tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isQuitKey(tt.key))
		})
	}
}

func TestDiffAnchor(t *testing.T) {
	tests := []struct {
		name       string
		prev, next string
		want       string
	}{
		{
			name: "picks longest new line",
			prev: "header\nfooter",
			next: "header\nshort new\na much longer new content line\nfooter",
			want: "a much longer new content line",
		},
		{
			name: "ignores borders and spinners",
			prev: "header",
			next: "header\n╭──────╮\n│ ⠋ thinking hard about this │\nreal answer",
			want: "real answer",
		},
		{
			name: "no new content",
			prev: "same\nlines",
			next: "same\nlines",
			want: "",
		},
		{
			name: "short lines are skipped",
			prev: "",
			next: "ab\nc",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, diffAnchor(tt.prev, tt.next))
		})
	}
}

func TestDiffAnchor_TruncatesLongLines(t *testing.T) {
	long := strings.Repeat("x", 100)
	got := diffAnchor("", long)
	assert.Len(t, got, maxAnchorRunes)
}

func TestWordAt(t *testing.T) {
	frame := "hello world\n  日本語 text"

	assert.Equal(t, "world", wordAt(frame, 8, 0))
	assert.Equal(t, "hello", wordAt(frame, 0, 0))
	// Wide runes: 日本語 occupies columns 2-7 on line 1.
	assert.Equal(t, "日本語", wordAt(frame, 4, 1))
	assert.Empty(t, wordAt(frame, 5, 0), "blank cell yields no word")
	assert.Empty(t, wordAt(frame, 0, 9), "out of bounds yields no word")
}

func TestClickCall_FallsBackToCoordinatesWhenAmbiguous(t *testing.T) {
	e := event{kind: clickEvent, x: 0, y: 0, frame: "copy\ncopy"}
	assert.Equal(t, "Click(0, 0)", clickCall(e))
}

func TestKeyCall(t *testing.T) {
	tests := []struct {
		key      tea.KeyPressMsg
		want     string
		needsTea bool
	}{
		{tea.KeyPressMsg{Code: tea.KeyEnter}, "Enter()", false},
		{tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, "Press(tea.KeyEnter, tea.ModAlt)", true},
		{tea.KeyPressMsg{Code: tea.KeyBackspace}, "Press(tea.KeyBackspace)", true},
		{tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}, "Press('k', tea.ModCtrl)", true},
		{tea.KeyPressMsg{Code: 'x'}, "Press('x')", false},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			call, needsTea := keyCall(tt.key)
			assert.Equal(t, tt.want, call)
			assert.Equal(t, tt.needsTea, needsTea)
		})
	}
}

func TestTestNameFromPath(t *testing.T) {
	assert.Equal(t, "TestRecordedMyScenario", TestNameFromPath("my-scenario.yaml"))
	assert.Equal(t, "TestRecordedCagentRecording17", TestNameFromPath("some/dir/cagent-recording-17.yaml"))

	// Names with no usable characters get a deterministic, distinct fallback.
	dashes := TestNameFromPath("---.yaml")
	underscores := TestNameFromPath("___.yaml")
	assert.Regexp(t, `^TestRecorded[0-9A-F]{8}$`, dashes)
	assert.Equal(t, dashes, TestNameFromPath("---.yaml"), "fallback must be deterministic")
	assert.NotEqual(t, dashes, underscores, "distinct paths must not collapse to the same test name")
}

func TestAgentBaseName(t *testing.T) {
	assert.Equal(t, "basic.yaml", agentBaseName("./e2e/testdata/basic.yaml"))
	assert.Equal(t, "agent.yml", agentBaseName("agent.yml"))
	// Default agent: no file argument at all.
	assert.Equal(t, "agent.yaml", agentBaseName(""))
	// Built-in, alias, or registry refs are not files.
	assert.Equal(t, "coder.yaml", agentBaseName("coder"))
	assert.Equal(t, "coder.yaml", agentBaseName("myorg/coder"))
}

func TestCommentSafe(t *testing.T) {
	assert.Equal(t, "a b c", commentSafe("a\nb\rc"))
	assert.Equal(t, "plain/path.yaml", commentSafe("plain/path.yaml"))
}

// TestGenerateTest_HostileNamesStayValidGo pins that control characters in
// user-controlled paths cannot break out of the generated header comment.
func TestGenerateTest_HostileNamesStayValidGo(t *testing.T) {
	r := &Recorder{inner: &fakeModel{}}
	r.events = append(r.events, typing(time.Now(), "frame", "hi")...)

	src := parseValid(t, r.GenerateTest(GenerateOptions{
		AgentFile:    "evil\npackage main//.yaml",
		CassettePath: "also\nevil.yaml",
	}))
	assert.NotContains(t, src, "\npackage main")
}
