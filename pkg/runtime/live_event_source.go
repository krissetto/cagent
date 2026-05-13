package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// LiveEventSource opens a live event stream for a session by id.
//
// It is the minimal surface a TUI / API client needs to attach to an
// arbitrary live session (root or subagent) without knowing whether the
// session is owned by an in-process runtime or served over HTTP.
//
// Callers must invoke the returned cancel function when finished to
// release subscription resources.
type LiveEventSource interface {
	AttachLiveSession(ctx context.Context, sessionID string) (<-chan Event, func(), error)
}

// LiveEventSourceWithSnapshot is the full live-attach surface: it returns
// the entire persisted session as a synthetic event prefix (so the caller
// can reconstruct the conversation regardless of attach timing) plus the
// live event tail.
type LiveEventSourceWithSnapshot interface {
	AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string) (snapshot []Event, ch <-chan Event, cancel func(), err error)
}

// Compile-time assertions.
var (
	_ LiveEventSource             = (*LocalRuntime)(nil)
	_ LiveEventSourceWithSnapshot = (*LocalRuntime)(nil)
)

// AttachLiveSession opens a live event stream for the given session id by
// subscribing to this runtime's per-session event bus. Use the returned
// cancel function to detach.
func (r *LocalRuntime) AttachLiveSession(ctx context.Context, sessionID string) (<-chan Event, func(), error) {
	if sessionID == "" {
		return nil, nil, errors.New("session id is required")
	}
	if r.eventBus == nil {
		return nil, nil, errors.New("event bus not configured")
	}
	sub := r.eventBus.Subscribe(ctx, sessionID, defaultEventChannelCapacity)
	return sub.Events, sub.Cancel, nil
}

// AttachLiveSessionWithSnapshot returns the full persisted transcript as a
// synthetic []Event prefix plus the live event tail. The snapshot is read
// from the configured session.Store (if any) before the subscription is
// established so that no event published while the snapshot is being read
// is lost: the live stream is started under the same event bus topic lock
// as the streaming-snapshot capture, and the session-store read is treated
// as the strictly older history.
//
// Deduplication: per-message events synthesized from the store carry their
// SessionPosition; when a streaming partial is in flight on the bus topic,
// it is folded into the snapshot prefix as an AgentChoiceEvent so the live
// tail can continue to emit new chunks without duplicating already-rendered
// content.
//
// This is the API behind the user-facing requirement that a client can
// attach to any session at any time and see the entire transcript plus
// live tail with no gap.
func (r *LocalRuntime) AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string) ([]Event, <-chan Event, func(), error) {
	if sessionID == "" {
		return nil, nil, nil, errors.New("session id is required")
	}
	if r.eventBus == nil {
		return nil, nil, nil, errors.New("event bus not configured")
	}

	// Read the persisted session first so the prefix is older than any
	// event the bus subscription will deliver. SubscribeWithSnapshot
	// captures a topic-locked streaming snapshot which is folded in below.
	persisted := r.snapshotEventsFromStore(ctx, sessionID)

	sub, streaming := r.eventBus.SubscribeWithSnapshot(ctx, sessionID, defaultEventChannelCapacity)

	// If the bus is currently mid-streaming an assistant turn that has not
	// yet been persisted, replay the accumulated content as an AgentChoice
	// event so the caller's UI can render the in-progress partial. The
	// matching MessageAddedEvent will arrive later on the live tail and
	// the recorder will persist it; we deliberately do not synthesize a
	// MessageAddedEvent here because the message is not finalized yet.
	if streaming.HasContent() {
		if streaming.ReasoningContent != "" {
			persisted = append(persisted, AgentChoiceReasoning(streaming.AgentName, sessionID, streaming.ReasoningContent))
		}
		if streaming.Content != "" {
			persisted = append(persisted, AgentChoice(streaming.AgentName, sessionID, streaming.Content))
		}
	}

	return persisted, sub.Events, sub.Cancel, nil
}

// snapshotEventsFromStore reads the persisted session and converts its
// items into a synthetic event sequence. Returns an empty slice when the
// store is missing the session or no store is configured. The returned
// events carry SessionPosition where applicable so positional dedup is
// possible against future persisted writes.
func (r *LocalRuntime) snapshotEventsFromStore(ctx context.Context, sessionID string) []Event {
	if r.sessionStore == nil {
		return nil
	}
	// Bound the store read so a slow/blocked store cannot stall attach.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	sess, err := r.sessionStore.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil
	}
	return synthesizeSnapshotEvents(sess)
}

// synthesizeSnapshotEvents converts a persisted session's items into the
// minimal sequence of events a client needs to reconstruct the transcript.
// The mapping is intentionally simple — a renderer that already understands
// the live event stream can reuse its existing handlers without special
// "snapshot" branches.
func synthesizeSnapshotEvents(sess *session.Session) []Event {
	if sess == nil {
		return nil
	}
	items := sess.Messages
	out := make([]Event, 0, len(items)+1)
	for i, item := range items {
		switch {
		case item.IsMessage():
			msg := item.Message
			if msg == nil {
				continue
			}
			switch msg.Message.Role {
			case chat.MessageRoleUser:
				out = append(out, UserMessage(msg.Message.Content, sess.ID, msg.Message.MultiContent, i))
			case chat.MessageRoleAssistant, chat.MessageRoleTool, chat.MessageRoleSystem:
				// Use MessageAdded which carries the full session.Message
				// payload (including tool calls and reasoning content).
				out = append(out, MessageAdded(sess.ID, msg, msg.AgentName, i))
			}
		case item.Summary != "":
			out = append(out, SessionSummary(sess.ID, item.Summary, "", item.FirstKeptEntry))
		case item.IsSubSession():
			// Sub-sessions completed in past runs are reported as a
			// completion event so clients can render them as collapsed
			// rows. Live-attach into the descendant subagent itself uses
			// AttachLiveSessionWithSnapshot on that session id directly.
			out = append(out, SubSessionCompleted(sess.ID, item.SubSession, ""))
		}
	}
	return out
}
