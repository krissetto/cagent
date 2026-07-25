package animation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runTick(t *testing.T, cmd func() any) TickMsg {
	t.Helper()
	msg, ok := cmd().(TickMsg)
	require.True(t, ok)
	return msg
}

func resetLegacyRuntime(t *testing.T) {
	t.Helper()
	legacyRuntime = NewRuntime()
	t.Cleanup(func() { legacyRuntime = NewRuntime() })
}

func TestLegacyFacadeContinuesOnDeliveredTickWithoutAccept(t *testing.T) {
	resetLegacyRuntime(t)

	first := StartTickIfFirst()
	require.NotNil(t, first)
	require.True(t, HasActive())

	firstTick := runTick(t, func() any { return first() })
	assert.Equal(t, 1, firstTick.Frame)
	assert.True(t, IsCurrentGen(firstTick))

	second := StartTick()
	require.NotNil(t, second, "legacy delivery must release the lease without Runtime.Accept")
	secondTick := runTick(t, func() any { return second() })
	assert.Equal(t, 2, secondTick.Frame)
	assert.True(t, IsCurrentGen(secondTick))

	Unregister()
	assert.False(t, HasActive())
	assert.Nil(t, StartTick())
}

func TestLegacyIsCurrentGenIsPureAndRejectsStaleTicks(t *testing.T) {
	resetLegacyRuntime(t)

	first := StartTickIfFirst()
	require.NotNil(t, first)
	firstTick := runTick(t, func() any { return first() })
	assert.True(t, IsCurrentGen(firstTick))
	assert.True(t, IsCurrentGen(firstTick), "generation checks are side-effect-free")

	second := StartTick()
	require.NotNil(t, second, "generation checks must not interfere with re-arming")
	Unregister()
	assert.False(t, IsCurrentGen(runTick(t, func() any { return second() })), "stopped chain is stale")
}

func TestAcceptedTickCopiesShareDirtyMarker(t *testing.T) {
	runtime := NewRuntime()
	sub := runtime.Subscribe()
	tick := runTick(t, func() any { return sub.Start()() })
	accepted, ok := runtime.Accept(tick)
	require.True(t, ok)
	acceptedCopy := accepted
	assert.False(t, accepted.Dirty())
	acceptedCopy.MarkDirty()
	assert.True(t, accepted.Dirty())
	elapsedBeforeTick, elapsedAfterTick := accepted.ElapsedBounds()
	assert.Less(t, elapsedBeforeTick, elapsedAfterTick)
	sub.Stop()
}

func TestRejectedTickCannotBecomeDirty(t *testing.T) {
	first, second := NewRuntime(), NewRuntime()
	sub := first.Subscribe()
	tick := runTick(t, func() any { return sub.Start()() })
	rejected, ok := second.Accept(tick)
	assert.False(t, ok)
	rejected.MarkDirty()
	assert.False(t, rejected.Dirty())
	sub.Stop()
}

func TestRuntimeIsolationAndOwnerToken(t *testing.T) {
	first, second := NewRuntime(), NewRuntime()
	firstSub, secondSub := first.Subscribe(), second.Subscribe()
	firstCmd := firstSub.Start()
	secondCmd := secondSub.Start()
	require.NotNil(t, firstCmd)
	require.NotNil(t, secondCmd)

	firstTick := runTick(t, func() any { return firstCmd() })
	secondTick := runTick(t, func() any { return secondCmd() })
	_, ok := second.Accept(firstTick)
	assert.False(t, ok, "runtime identity rejects another runtime's tick")
	assert.Zero(t, second.Now())
	_, ok = first.Accept(firstTick)
	require.True(t, ok)
	assert.Positive(t, first.Now())
	assert.Zero(t, second.Now())
	_, ok = second.Accept(secondTick)
	require.True(t, ok)
}

func TestRuntimeStopDoesNotAffectAnotherRuntime(t *testing.T) {
	first, second := NewRuntime(), NewRuntime()
	firstSub, secondSub := first.Subscribe(), second.Subscribe()
	firstCmd, secondCmd := firstSub.Start(), secondSub.Start()
	firstSub.Stop()
	assert.False(t, first.HasActive())
	assert.True(t, second.HasActive())
	assert.Nil(t, first.Continue())
	secondTick := runTick(t, func() any { return secondCmd() })
	_, ok := second.Accept(secondTick)
	require.True(t, ok)
	assert.NotNil(t, second.Continue())
	secondSub.Stop()
	_ = firstCmd
}

func TestRuntimeAcceptOnlyOnce(t *testing.T) {
	runtime := NewRuntime()
	sub := runtime.Subscribe()
	tick := runTick(t, func() any { return sub.Start()() })
	_, ok := runtime.Accept(tick)
	require.True(t, ok)
	elapsed := runtime.Now()
	_, ok = runtime.Accept(tick)
	assert.False(t, ok)
	assert.Equal(t, elapsed, runtime.Now())
	sub.Stop()
}

func TestRuntimeTransitionUsesOwnedClock(t *testing.T) {
	runtime := NewRuntime()
	transition := runtime.Transition()
	cmd := transition.Start(TickRate, Linear)
	tick := runTick(t, func() any { return cmd() })
	_, ok := runtime.Accept(tick)
	require.True(t, ok)
	transition.Tick()
	assert.False(t, transition.Running())
	assert.Equal(t, int32(0), runtime.ActiveCount())
}

func TestTickOwnerIsImmutableAcrossRecovery(t *testing.T) {
	runtime := NewRuntime()
	sub := runtime.Subscribe()
	staleCmd := sub.Start()
	freshCmd := runtime.EnsureRunning()
	stale := runTick(t, func() any { return staleCmd() })
	fresh := runTick(t, func() any { return freshCmd() })
	_, ok := runtime.Accept(stale)
	assert.False(t, ok)
	_, ok = runtime.Accept(fresh)
	assert.True(t, ok)
	sub.Stop()
}

func TestRuntimeStopInvalidatesQueuedTickAndQuiesces(t *testing.T) {
	runtime := NewRuntime()
	sub := runtime.Subscribe()
	queued := sub.Start()
	require.NotNil(t, queued)

	runtime.Stop()
	assert.Equal(t, int32(0), runtime.ActiveCount())
	assert.Nil(t, runtime.Continue(), "stopped animation runtime schedules no successor")

	_, accepted := runtime.Accept(runTick(t, func() any { return queued() }))
	assert.False(t, accepted, "queued generation is stale after teardown")
	assert.Zero(t, runtime.Now(), "rejected queued tick does not advance the clock")
}

func TestExactlyOneContinuationPerAcceptedTick(t *testing.T) {
	runtime := NewRuntime()
	sub := runtime.Subscribe()
	first := sub.Start()
	_, accepted := runtime.Accept(runTick(t, func() any { return first() }))
	require.True(t, accepted)

	successor := runtime.Continue()
	require.NotNil(t, successor)
	assert.Nil(t, runtime.Continue(), "a live lease deduplicates parallel continuations")
	sub.Stop()
	_, accepted = runtime.Accept(runTick(t, func() any { return successor() }))
	assert.False(t, accepted, "last unregister invalidates queued successor")
	assert.Nil(t, runtime.Continue())
}

func TestTabBusySpinnerIsOneCellBraille(t *testing.T) {
	for _, frame := range TabBusy.Frames() {
		runes := []rune(frame)
		require.Len(t, runes, 1)
		assert.GreaterOrEqual(t, runes[0], rune(0x2800))
		assert.LessOrEqual(t, runes[0], rune(0x28ff))
	}
}

func TestRuntimeNowAdvancesByDeliveredTime(t *testing.T) {
	runtime := NewRuntime()
	runtime.Register()
	base := time.Now()
	runtime.mu.Lock()
	runtime.tickScheduled = true
	runtime.generation = 2
	runtime.lastDeliveredAt = base
	runtime.mu.Unlock()
	_, ok := runtime.Accept(TickMsg{runtimeIdentity: runtime.runtimeIdentity, generation: 2, deliveredAt: base.Add(125 * time.Millisecond)})
	require.True(t, ok)
	assert.Equal(t, 125*time.Millisecond, runtime.Now())
	runtime.Unregister()
}
