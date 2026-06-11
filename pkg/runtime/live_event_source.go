package runtime

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// LiveEventSource opens a live event stream for a session by id.
type LiveEventSource interface {
	AttachLiveSession(ctx context.Context, sessionID string) (<-chan Event, func(), error)
}

// LiveEventSourceWithSnapshot returns an event snapshot prefix and live tail
// for an arbitrary live session in this runtime tree.
type LiveEventSourceWithSnapshot interface {
	AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string, buffer int) ([]Event, <-chan Event, error)
}

// LiveSessionRuntime aggregates the live observability and control surface for
// root sessions and runtime-managed descendants.
type LiveSessionRuntime interface {
	LiveEventSource
	LiveEventSourceWithSnapshot

	LiveSessionTree(ctx context.Context, sessionID string) (*LiveSessionTree, error)
	LiveChildSession(sessionID string) (*session.Session, bool)
	SteerSessionByID(sessionID string, msg QueuedMessage) error
	FollowUpSessionByID(sessionID string, msg QueuedMessage) error
	InterruptSessionByID(sessionID string) error
	CloseSessionByID(sessionID string) error
	StopSessionByID(sessionID string) error
}

var _ LiveSessionRuntime = (*LocalRuntime)(nil)

func (r *LocalRuntime) AttachLiveSession(ctx context.Context, sessionID string) (<-chan Event, func(), error) {
	if r == nil || r.eventBus == nil {
		return nil, nil, ErrLiveSessionUnavailable
	}
	if _, err := r.resolveSessionControl(sessionID); err != nil {
		return nil, nil, err
	}
	sub := r.eventBus.Subscribe(ctx, sessionID, defaultEventChannelCapacity)
	return sub.Events(), sub.Close, nil
}

func (r *LocalRuntime) AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string, buffer int) ([]Event, <-chan Event, error) {
	if r == nil || r.eventBus == nil {
		return nil, nil, ErrLiveSessionUnavailable
	}
	if _, err := r.resolveSessionControl(sessionID); err != nil {
		return nil, nil, err
	}
	sub, snapshot := r.eventBus.SubscribeWithSnapshot(ctx, sessionID, buffer)
	if queueEvent := r.sessionQueueSnapshotEvent(sessionID); queueEvent != nil {
		snapshot = append(snapshot, queueEvent)
	}
	return snapshot, sub.Events(), nil
}

func (r *LocalRuntime) LiveChildSession(sessionID string) (*session.Session, bool) {
	h, err := r.resolveSubagentSession(sessionID)
	if err != nil || h == nil || h.sess == nil {
		return nil, false
	}
	return h.sess, true
}

func (r *LocalRuntime) SteerSessionByID(sessionID string, msg QueuedMessage) error {
	target, err := r.resolveSessionControl(sessionID)
	if err != nil {
		return err
	}
	if target.root {
		return r.Steer(msg)
	}
	queues := r.childSessionQueues(target.handle)
	if !queues.steer.Enqueue(context.Background(), msg) {
		return errors.New("steer queue full")
	}
	target.handle.signal()
	return nil
}

func (r *LocalRuntime) FollowUpSessionByID(sessionID string, msg QueuedMessage) error {
	target, err := r.resolveSessionControl(sessionID)
	if err != nil {
		return err
	}
	if target.root {
		return r.FollowUp(msg)
	}
	queues := r.childSessionQueues(target.handle)
	if !queues.followUp.Enqueue(context.Background(), msg) {
		return errors.New("follow-up queue full")
	}
	r.publishQueueSnapshot(sessionID, queues.followUp)
	target.handle.signal()
	return nil
}

func (r *LocalRuntime) InterruptSessionByID(sessionID string) error {
	target, err := r.resolveSessionControl(sessionID)
	if err != nil {
		return err
	}
	if target.root {
		return errors.New("interrupt by id is only supported for runtime-managed child sessions")
	}
	select {
	case <-target.handle.stop:
	default:
		close(target.handle.stop)
	}
	return nil
}

func (r *LocalRuntime) CloseSessionByID(sessionID string) error {
	target, err := r.resolveSessionControl(sessionID)
	if err != nil {
		return err
	}
	if target.root {
		return errors.New("close by id is only supported for runtime-managed child sessions")
	}
	return target.handle.finalize()
}

func (r *LocalRuntime) StopSessionByID(sessionID string) error {
	target, err := r.resolveSessionControl(sessionID)
	if err != nil {
		return err
	}
	if target.root {
		return errors.New("stop by id is only supported for runtime-managed child sessions")
	}
	return target.handle.stopNow()
}

type sessionControlTarget struct {
	root   bool
	handle *subagentHandle
}

func (r *LocalRuntime) resolveSessionControl(sessionID string) (sessionControlTarget, error) {
	if sessionID == "" {
		return sessionControlTarget{}, errors.New("session id is required")
	}
	if h, err := r.resolveSubagentSession(sessionID); err == nil {
		if !r.isSameRoot(h.sess) {
			return sessionControlTarget{}, errors.New("cross-root subagent access rejected")
		}
		return sessionControlTarget{handle: h}, nil
	} else if !errors.Is(err, ErrLiveSessionUnavailable) {
		return sessionControlTarget{}, err
	}
	if r.isLiveRootSession(sessionID) {
		return sessionControlTarget{root: true}, nil
	}
	return sessionControlTarget{}, ErrLiveSessionUnavailable
}

func (r *LocalRuntime) resolveSubagentSession(sessionID string) (*subagentHandle, error) {
	if r == nil || r.subagents == nil || sessionID == "" {
		return nil, ErrLiveSessionUnavailable
	}
	return r.subagents.ResolveSession(sessionID)
}

func (r *LocalRuntime) isSameRoot(sess *session.Session) bool {
	if sess == nil || r == nil || r.liveSessions == nil {
		return false
	}
	root := sess.EffectiveRootID()
	entry, ok := r.liveSessions.get(root)
	return ok && entry.ParentID == ""
}

func (r *LocalRuntime) isLiveRootSession(sessionID string) bool {
	if r == nil || r.liveSessions == nil || sessionID == "" {
		return false
	}
	entry, ok := r.liveSessions.get(sessionID)
	return ok && entry.ParentID == ""
}

func (r *LocalRuntime) childSessionQueues(h *subagentHandle) sessionQueues {
	if h == nil || h.sess == nil {
		return sessionQueues{}
	}
	return r.queuesFor(h.sess)
}

func (r *LocalRuntime) sessionQueueSnapshotEvent(sessionID string) Event {
	target, err := r.resolveSessionControl(sessionID)
	if err != nil || target.root || target.handle == nil || target.handle.sess == nil {
		return nil
	}
	queues := r.queuesFor(target.handle.sess)
	snapshot, ok := queues.followUp.(QueueSnapshotter)
	if !ok {
		return nil
	}
	queued := snapshot.Snapshot()
	if len(queued) == 0 {
		return nil
	}
	previews := make([]string, 0, len(queued))
	for _, msg := range queued {
		previews = append(previews, queuedMessagePreview(msg))
	}
	return SessionQueue(sessionID, len(previews), previews)
}

func queuedMessagePreview(msg QueuedMessage) string {
	if msg.Content != "" {
		return msg.Content
	}
	for _, part := range msg.MultiContent {
		if part.Type == chat.MessagePartTypeText && part.Text != "" {
			return part.Text
		}
	}
	if len(msg.MultiContent) > 0 {
		return "[multimodal message]"
	}
	return ""
}
