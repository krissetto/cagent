package tools

import (
	"context"
	"errors"
)

// Capability identifies an optional runtime facility a tool can probe for
// via [Runtime.Supports] before relying on it.
type Capability string

const (
	// CapabilityOutput indicates [Runtime.EmitOutput] streams incremental
	// output to a live consumer (UI, event feed).
	CapabilityOutput Capability = "output"
	// CapabilityRecall indicates [Runtime.Recall] can inject messages into
	// a running agent loop.
	CapabilityRecall Capability = "recall"
)

// Runtime is the tool-side handle to the hosting agent runtime, passed
// explicitly to every [ToolHandler]. Implementations must remain valid after
// the handler returns so background work can hold the handle; every method
// takes ctx from the caller instead of capturing one.
//
// Hosts without an agent loop pass [NopRuntime].
type Runtime interface {
	// EmitOutput streams incremental output for the current tool call.
	EmitOutput(ctx context.Context, output string)
	// Recall injects a message into the agent loop, waking it if idle.
	// Typically called when background work completes after the tool call
	// that started it has already returned.
	Recall(ctx context.Context, message string) error
	// Supports reports whether the host provides the named capability,
	// letting tools fail fast before starting background work.
	Supports(capability Capability) bool
}

// ErrRecallNotSupported is returned by [Runtime.Recall] implementations that
// cannot steer an agent loop.
var ErrRecallNotSupported = errors.New("recall is not supported by this host")

// ErrCallTimeout is returned when a tool call exceeds its configured
// call_timeout. Wrapped with the elapsed duration via fmt.Errorf("%w after
// %s", ErrCallTimeout, ...) — use errors.Is to detect it.
var ErrCallTimeout = errors.New("tool call timed out")

// NopRuntime is the [Runtime] for hosts without an agent loop (slash-command
// expansion, prompt templates, tests). EmitOutput is a no-op, Recall fails
// with [ErrRecallNotSupported], and no capability is supported.
type NopRuntime struct{}

var _ Runtime = NopRuntime{}

func (NopRuntime) EmitOutput(context.Context, string) {}

func (NopRuntime) Recall(context.Context, string) error { return ErrRecallNotSupported }

func (NopRuntime) Supports(Capability) bool { return false }
