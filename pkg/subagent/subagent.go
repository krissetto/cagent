// Package subagent provides the runtime-native subagent subsystem.
//
// It is designed around the following core ideas:
//
//  1. Every subagent has its own [session.Session].
//  2. Subagents run in the background on their own goroutine and keep
//     a persistent, conversational loop alive until they are explicitly
//     closed or stopped by the parent.
//  3. A subagent that completes a turn publishes a compact [Envelope]
//     into its parent's mailbox. The parent loop consumes envelopes at
//     safe points (mid-turn, between tool batches) or blocks on them
//     once the parent's own turn has ended.
//  4. The parent can send messages to any live subagent at any time.
//     Subagents deliver those messages to their loop at safe points
//     (mirroring how user steer messages are delivered to the root loop).
//
// The package is runtime-agnostic: it uses only [session.Session] and a
// small [Runner] interface that the runtime implements. This keeps
// subagent core logic easy to test in isolation, and keeps the runtime
// package free of subagent lifecycle bookkeeping.
package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// Status describes the lifecycle state of a managed subagent.
type Status int32

const (
	// StatusStarting is the initial state, before the child loop begins.
	StatusStarting Status = iota
	// StatusRunning means the child loop is actively processing a turn.
	StatusRunning
	// StatusWaiting means the child loop has finished a turn and is
	// awaiting a new message from its parent (keep-alive mode).
	StatusWaiting
	// StatusClosed means the child loop exited cleanly at the parent's
	// request.
	StatusClosed
	// StatusStopped means the child was cancelled (context or explicit
	// stop) before completing naturally.
	StatusStopped
	// StatusFailed means the child exited because of an unrecoverable
	// runtime error.
	StatusFailed
)

// String returns a stable lowercase label for the status, suitable for
// use in events, logs, and tool output.
func (s Status) String() string {
	switch s {
	case StatusStarting:
		return "starting"
	case StatusRunning:
		return "running"
	case StatusWaiting:
		return "waiting"
	case StatusClosed:
		return "closed"
	case StatusStopped:
		return "stopped"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// IsTerminal reports whether the status is one the subagent can no
// longer transition out of.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusClosed, StatusStopped, StatusFailed:
		return true
	default:
		return false
	}
}

// UpdateKind classifies an [Envelope] so callers can quickly tell what
// kind of wake-up they are receiving without inspecting Status.
type UpdateKind string

const (
	// UpdateKindTurnCompleted means the subagent finished a turn and is
	// awaiting further instructions from the parent.
	UpdateKindTurnCompleted UpdateKind = "turn_completed"
	// UpdateKindClosed means the subagent exited after a parent-initiated
	// close.
	UpdateKindClosed UpdateKind = "closed"
	// UpdateKindStopped means the subagent exited because of explicit
	// cancellation.
	UpdateKindStopped UpdateKind = "stopped"
	// UpdateKindFailed means the subagent exited because of a runtime
	// error. Error carries the failure detail.
	UpdateKindFailed UpdateKind = "failed"
	// UpdateKindStatusOnly means only the subagent status changed. It is
	// intentionally sidebar-only: parent UIs should refresh row state without
	// adding a transcript card or waking the parent loop as if a turn completed.
	UpdateKindStatusOnly UpdateKind = "status_only"
)

// MessageMode controls how a parent→child message is delivered into the
// subagent's loop. It mirrors the steer/follow-up split that the user has
// available against the root agent.
type MessageMode string

const (
	// MessageModeFollowUp delivers the message as a plain user turn between
	// the child's turns. The zero value is follow-up for backward
	// compatibility.
	MessageModeFollowUp MessageMode = ""
	// MessageModeSteer delivers the message at the next safe point during a
	// running child turn (or, if the child is parked, at the next parked
	// wake-up). It is the parent-agent counterpart of the user-facing Steer
	// API.
	MessageModeSteer MessageMode = "steer"
)

// Message is a user-role message to be injected into a subagent's loop.
// It mirrors the shape of a minimal user turn (text plus optional
// multi-modal parts).
//
// Mode controls which inbox the message is routed to. The zero value is
// [MessageModeFollowUp].
type Message struct {
	Content      string
	MultiContent []chat.MessagePart
	Mode         MessageMode
}

// Envelope is the compact, runtime-generated payload delivered to a
// parent when a subagent produces an observable update.
//
// The Preview field holds a truncated snippet of the subagent's last
// assistant message so the parent has enough signal to decide what to
// do next without blowing up its own context window. The full message
// can always be retrieved from the subagent session if needed.
type Envelope struct {
	// SubAgentID is the stable identifier of the subagent that emitted
	// this update. It matches the child session ID.
	SubAgentID string
	// ParentSessionID is the identifier of the parent session that owns
	// this subagent.
	ParentSessionID string
	// AgentName is the configured agent name driving the subagent.
	AgentName string
	// Kind classifies the update.
	Kind UpdateKind
	// Status snapshots the child's status at the time the envelope was
	// produced. It is always a terminal status for closed/stopped/failed
	// envelopes and [StatusWaiting] for turn-completed envelopes.
	Status Status
	// Preview is a short excerpt of the subagent's last assistant
	// message. It is always safe to include directly in the parent's
	// conversation.
	Preview string
	// Truncated reports whether Preview was shortened.
	Truncated bool
	// Error holds the runtime error message for [UpdateKindFailed].
	Error string
	// At is the wall-clock time at which the envelope was generated.
	At time.Time
}

// HandleSnapshot is a read-only view of a subagent's state that is safe
// to return from accessor methods. It never exposes internal channels.
type HandleSnapshot struct {
	ID              string
	ParentSessionID string
	AgentName       string
	Title           string
	Intent          string
	ExternalID      string
	Status          Status
	Depth           int
	CreatedAt       time.Time
	LastUpdateAt    time.Time
	LastPreview     string
	Error           string
}

// StartConfig is the parameter block for [Manager.StartChild].
//
// Everything except Parent and AgentName is optional.
type StartConfig struct {
	// Parent is the calling parent session.
	Parent *session.Session
	// AgentName is the configured agent that will run the subagent.
	AgentName string
	// Task is a short human description of the work the subagent should
	// carry out. It is rendered into the child's initial user message.
	Task string
	// InitialMessage is the very first user-role message delivered to
	// the subagent. When empty the manager synthesises a neutral
	// "Please proceed." message to kick things off.
	InitialMessage Message
	// Title is a human-readable label for the child session.
	Title string
	// Intent is an optional short, machine-readable label describing the
	// reason for spawning this subagent (e.g. "delegate", "research",
	// "remote-mirror"). Alternative runners can carry this through to
	// external observability tools so they see the same semantic edge label
	// that local consumers see.
	Intent string
	// ToolsApproved controls whether the child session runs with tools
	// pre-approved. Background subagents typically need this because
	// there is no user present to answer confirmations.
	ToolsApproved bool
	// MaxPreview caps the envelope preview length. 0 uses
	// [DefaultPreviewLimit].
	MaxPreview int
	// ExcludedTools is forwarded to the child session so filtering
	// (e.g. run_skill recursion) flows through nested trees.
	ExcludedTools []string
}

// Runner is the narrow contract the manager requires from a child-loop
// implementation. Keeping this interface small makes the manager easy to
// test with a fake runner and allows alternate local or remote runners to
// plug in without changing Manager or Handle.
//
// Contractually, a Runner owns only the child-loop orchestration. It must:
//   - react to parent→child wake-ups from [Handle.InboxSignal] and
//     [Handle.SteerInboxSignal], draining them with [Handle.DrainInbox]
//     / [Handle.DrainSteerInbox];
//   - react to finalize/stop requests from [Handle.CloseCh];
//   - publish child lifecycle/output back to the manager exclusively through
//     [Handle.PublishTurn], [Handle.PublishFailure], [Handle.PublishClosed],
//     and [Handle.PublishStopped].
//
// A Runner must not reach into Manager internals or mutate Handle state in
// any other way.
type Runner interface {
	// StartChildLoop starts the subagent's keep-alive loop and returns
	// immediately. The returned channel closes when the loop has fully
	// exited (ctx cancel, close requested, or terminal failure).
	//
	// The loop is expected to:
	//   * react to wake-ups from h.InboxSignal() / h.SteerInboxSignal() at
	//     safe points and call h.DrainInbox() / h.DrainSteerInbox() to
	//     collect parent→child messages;
	//   * call h.PublishTurn() / PublishFailure() / PublishClosed() /
	//     PublishStopped() exactly at the points described by those
	//     methods' documentation;
	//   * stop running as soon as h.CloseCh() fires, ctx is cancelled, or
	//     it observes a terminal failure.
	StartChildLoop(ctx context.Context, h *Handle) <-chan struct{}
}

// ClosedError is returned by Manager operations that target a subagent
// that has already left its live state.
type ClosedError struct {
	ID     string
	Status Status
}

func (e *ClosedError) Error() string {
	return "subagent " + e.ID + " is " + e.Status.String()
}

// NotFoundError is returned when a subagent id is unknown to the manager.
type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return "subagent " + e.ID + " not found"
}

// AmbiguousRefError is returned by [Manager.ResolveChildRef] when a short ref
// matches more than one live child of the same parent. It carries the
// short refs of every matching candidate so callers can surface a helpful
// error to the model.
type AmbiguousRefError struct {
	Ref        string
	Candidates []string
}

func (e *AmbiguousRefError) Error() string {
	return "subagent ref " + e.Ref + " is ambiguous; matches: " + strings.Join(e.Candidates, ", ")
}

// DepthExceededError is returned when starting a subagent would exceed the
// runtime's configured recursion cap. Attempted is the depth that would
// result from the rejected StartChild call (1 = root's direct child).
type DepthExceededError struct {
	Limit     int
	Attempted int
}

func (e *DepthExceededError) Error() string {
	return fmt.Sprintf("subagent depth limit exceeded: attempted depth %d, limit %d", e.Attempted, e.Limit)
}

// DescendantLimitError is returned when starting a subagent would exceed the
// total number of descendants allowed within a single root-session tree.
type DescendantLimitError struct {
	Limit int
}

func (e *DescendantLimitError) Error() string {
	return fmt.Sprintf("subagent descendant limit exceeded: limit %d", e.Limit)
}

// ShutdownTimeoutError is returned when [Manager.StopAll] is asked to
// wait for live subagent loops to exit but the supplied context expires
// first.
type ShutdownTimeoutError struct {
	Lingering []string
}

func (e *ShutdownTimeoutError) Error() string {
	return "timed out waiting for subagents to stop: " + strings.Join(e.Lingering, ", ")
}

// DefaultPreviewLimit is the default maximum length (in runes) of the
// Preview field on an [Envelope]. Chosen to be large enough to be
// useful for typical agent responses yet small enough to keep the
// parent's context budget healthy.
const DefaultPreviewLimit = 320

// DefaultMaxDepth caps how deep a runtime-managed subagent tree can get.
// Depth 1 = root's direct children. Depth 2 = grandchildren. And so on.
// Chosen as a balance between supporting legitimate nested workflows and
// catching runaway recursion quickly.
const DefaultMaxDepth = 8

// DefaultMaxDescendants caps the total number of live descendants
// (children + grandchildren + ...) under a single root session tree.
const DefaultMaxDescendants = 64

// atomicString is a tiny helper around atomic.Value that stores a
// string. It returns "" for unset values.
//
// We keep this wrapper instead of switching to atomic.Pointer[string] so
// stores stay allocation-free for ordinary string updates on hot metadata
// paths like title, preview, and error mirroring.
type atomicString struct{ v atomic.Value }

func (a *atomicString) Store(s string) { a.v.Store(s) }
func (a *atomicString) Load() string {
	if v := a.v.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// atomicTime stores a time.Time atomically.
//
// Like atomicString, this remains a small atomic.Value wrapper so callers can
// Store concrete values directly without introducing pointer allocation churn.
type atomicTime struct{ v atomic.Value }

func (a *atomicTime) Store(t time.Time) { a.v.Store(t) }
func (a *atomicTime) Load() time.Time {
	if v := a.v.Load(); v != nil {
		return v.(time.Time)
	}
	return time.Time{}
}
