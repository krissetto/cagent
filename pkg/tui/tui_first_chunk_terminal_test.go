package tui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	vt10x "github.com/ActiveState/vt10x"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type terminalRecorder struct {
	mu  sync.Mutex
	raw bytes.Buffer
	vt  *vt10x.VT
	st  *vt10x.State
}

func newTerminalRecorder(width, height int) *terminalRecorder {
	st := &vt10x.State{}
	vt, err := vt10x.New(st, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		panic(err)
	}
	vt.Resize(width, height)
	return &terminalRecorder{vt: vt, st: st}
}

func (r *terminalRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raw.Write(p)
	return r.vt.Write(p)
}

func (r *terminalRecorder) snapshot() (raw, screen string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.raw.String(), r.st.String()
}

func TestActualProgramFirstChunkReplacesPrimedSpinnerWithoutClick(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	recorder := newTerminalRecorder(120, 40)
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(recorder), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready
	require.Eventually(t, func() bool {
		raw, _ := recorder.snapshot()
		return raw != ""
	}, time.Second, time.Millisecond)
	programAck(t, program)

	beforeFrame := programFrame(t, program)
	beforeRaw, beforeScreen := recorder.snapshot()
	beforeGeometry := root.chatPage.(interface {
		GeometryForTest() messagecomponent.GeometryForTest
	}).GeometryForTest()
	beforeGeneration, beforeValid := root.chatPage.VisualGeneration(), root.viewCacheValid

	program.Send(agentruntime.AgentChoice("root", "profile", "FIRST-CHUNK-MARKER\n\n"))
	programAck(t, program)
	require.Eventually(t, func() bool {
		_, screen := recorder.snapshot()
		return strings.Contains(screen, "FIRST-CHUNK-MARKER")
	}, time.Second, time.Millisecond)
	failedFrame := programFrame(t, program)
	failedRaw, failedScreen := recorder.snapshot()
	failedGeometry := root.chatPage.(interface {
		GeometryForTest() messagecomponent.GeometryForTest
	}).GeometryForTest()
	failedGeneration, failedValid := root.chatPage.VisualGeneration(), root.viewCacheValid

	// Mandatory inert recovery click: this currently exposes the stale root cache.
	program.Send(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 39})
	programAck(t, program)
	recoveredFrame := programFrame(t, program)
	recoveredRaw, recoveredScreen := recorder.snapshot()
	recoveredGeometry := root.chatPage.(interface {
		GeometryForTest() messagecomponent.GeometryForTest
	}).GeometryForTest()
	recoveredGeneration, recoveredValid := root.chatPage.VisualGeneration(), root.viewCacheValid

	t.Logf("before cache=%v gen=%d geom=%+v frameMarker=%v screenMarker=%v rawBytes=%d", beforeValid, beforeGeneration, beforeGeometry, strings.Contains(ansi.Strip(beforeFrame), "FIRST-CHUNK-MARKER"), strings.Contains(beforeScreen, "FIRST-CHUNK-MARKER"), len(beforeRaw))
	t.Logf("failed cache=%v gen=%d geom=%+v frameMarker=%v screenMarker=%v rawBytes=%d rowsStartingScrollbar=%d", failedValid, failedGeneration, failedGeometry, strings.Contains(ansi.Strip(failedFrame), "FIRST-CHUNK-MARKER"), strings.Contains(failedScreen, "FIRST-CHUNK-MARKER"), len(failedRaw), rowsStartingScrollbar(failedScreen))
	t.Logf("recovered cache=%v gen=%d geom=%+v frameMarker=%v screenMarker=%v rawBytes=%d rowsStartingScrollbar=%d", recoveredValid, recoveredGeneration, recoveredGeometry, strings.Contains(ansi.Strip(recoveredFrame), "FIRST-CHUNK-MARKER"), strings.Contains(recoveredScreen, "FIRST-CHUNK-MARKER"), len(recoveredRaw), rowsStartingScrollbar(recoveredScreen))

	require.Contains(t, ansi.Strip(failedFrame), "FIRST-CHUNK-MARKER", "first chunk must render immediately without a click")
	require.Contains(t, failedScreen, "FIRST-CHUNK-MARKER", "terminal must receive first chunk immediately")
	require.Equal(t, visibleGeometry(failedGeometry), visibleGeometry(recoveredGeometry), "inert recovery click must not change visible geometry")
	require.Contains(t, ansi.Strip(recoveredFrame), "FIRST-CHUNK-MARKER")

	program.Quit()
	require.NoError(t, <-done)
	root.animationRuntime.Stop()
}

func rowsStartingScrollbar(screen string) int {
	n := 0
	for line := range strings.SplitSeq(screen, "\n") {
		if strings.HasPrefix(line, "│") || strings.HasPrefix(line, "┃") || strings.HasPrefix(line, "█") {
			n++
		}
	}
	return n
}
