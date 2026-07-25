package animation

import tea "charm.land/bubbletea/v2"

// Subscription represents a component's subscription to animation ticks.
// It encapsulates the registration/unregistration lifecycle, making it
// easier to manage animation state correctly.
//
// Usage:
//
//	type MyComponent struct {
//	    animSub animation.Subscription
//	}
//
//	func (m *MyComponent) Init() tea.Cmd {
//	    return m.animSub.Start()
//	}
//
//	func (m *MyComponent) Cleanup() {
//	    m.animSub.Stop()
//	}
type Subscription struct {
	active  bool
	runtime *Runtime
}

// NewSubscription returns an inactive subscription owned by an animation runtime.
func NewSubscription(runtime *Runtime) Subscription {
	if runtime == nil {
		panic("animation: nil runtime")
	}
	return Subscription{runtime: runtime}
}

// SetRuntime binds an inactive subscription to a program's animation runtime.
func (s *Subscription) SetRuntime(runtime *Runtime) {
	if runtime == nil {
		panic("animation: nil runtime")
	}
	if s.active {
		panic("animation: cannot rebind active subscription")
	}
	s.runtime = runtime
}

func (s *Subscription) boundRuntime() *Runtime {
	if s.runtime == nil {
		// Preserve zero-value Subscription behavior for components that have not
		// yet adopted explicit program ownership.
		return legacyRuntime
	}
	return s.runtime
}

// Start activates the subscription if not already active.
// Returns a command to start the tick if this is the first subscription.
// Safe to call multiple times - only the first call registers.
func (s *Subscription) Start() tea.Cmd {
	if s.active {
		return nil
	}
	s.active = true
	if s.runtime == nil {
		return legacyRuntime.legacyStart()
	}
	return s.runtime.start()
}

// Stop deactivates the subscription if currently active.
// Safe to call multiple times - only the first call unregisters.
func (s *Subscription) Stop() {
	if !s.active {
		return
	}
	s.active = false
	s.boundRuntime().Unregister()
}

// IsActive returns whether the subscription is currently active.
func (s *Subscription) IsActive() bool {
	return s.active
}

// Reset returns a new inactive subscription.
// Useful when recreating a component that needs fresh animation state.
func (s *Subscription) Reset() Subscription {
	runtime := s.runtime
	s.Stop()
	return Subscription{runtime: runtime}
}
