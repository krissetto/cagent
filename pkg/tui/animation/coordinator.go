// Package animation provides centralized animation tick management for the TUI.
// All animated components (spinners, fades, etc.) share a single tick stream
// to avoid tick storms and ensure synchronized animations.
//
// Thread safety: All exported functions are safe for concurrent use, though the
// typical usage pattern is single-threaded via Bubble Tea's Update loop.
package animation

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TickMsg is broadcast to all animated components on each animation frame.
// Components should handle this message to update their animation state.
type TickMsg struct {
	Frame int
}

// Coordinator manages a single tick stream for all animations.
// It tracks active animations and only generates ticks when at least one is active.
type Coordinator struct {
	// mu guards all fields. While Bubble Tea's Update loop is single-threaded,
	// the mutex protects against accidental misuse from Cmd goroutines and
	// ensures StartTickIfFirst is atomic (no race between check and register).
	mu     sync.Mutex
	frame  int
	active int32
}

// Register increments the active animation count.
// Call this when an animation starts.
func (c *Coordinator) Register() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active++
}

// Unregister decrements the active animation count.
// Call this when an animation stops.
func (c *Coordinator) Unregister() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active > 0 {
		c.active--
	}
}

// HasActive returns true if any animations are currently active.
func (c *Coordinator) HasActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active > 0
}

// StartTick starts the animation tick if any animations are active.
// Call this after processing a TickMsg to continue the tick stream.
func (c *Coordinator) StartTick() tea.Cmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active <= 0 {
		return nil
	}
	return c.tickLocked()
}

// StartTickIfFirst registers an animation and starts the tick if this is the first.
// This is atomic: no race between checking and registering.
// Returns the tick command if the tick stream was started, nil otherwise.
func (c *Coordinator) StartTickIfFirst() tea.Cmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasEmpty := c.active == 0
	c.active++
	if wasEmpty {
		return c.tickLocked()
	}
	return nil
}

// tickLocked returns a tick command. Must be called with mu held.
// 14 FPS - smooth enough for most animations without being too CPU-intensive.
func (c *Coordinator) tickLocked() tea.Cmd {
	return tea.Tick(time.Second/14, func(time.Time) tea.Msg {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.frame++
		frame := c.frame
		return TickMsg{Frame: frame}
	})
}

// globalCoordinator is the singleton coordinator instance shared by all
// animated components in the running TUI.
var globalCoordinator = &Coordinator{}

// Unregister decrements the active animation count on the global coordinator.
func Unregister() { globalCoordinator.Unregister() }

// HasActive reports whether any animations are active on the global coordinator.
func HasActive() bool { return globalCoordinator.HasActive() }

// StartTick starts the global animation tick if any animations are active.
func StartTick() tea.Cmd { return globalCoordinator.StartTick() }

// StartTickIfFirst registers an animation on the global coordinator and starts
// the tick if it is the first.
func StartTickIfFirst() tea.Cmd { return globalCoordinator.StartTickIfFirst() }
