package runtime

import "context"

func (r *LocalRuntime) AttachLiveSessionWithSnapshot(ctx context.Context, sessionID string, buffer int) ([]Event, <-chan Event, error) {
	if r == nil || r.eventBus == nil {
		return nil, nil, ErrLiveSessionUnavailable
	}
	if _, ok := r.liveSessions.get(sessionID); !ok {
		return nil, nil, ErrLiveSessionUnavailable
	}
	sub, snapshot := r.eventBus.SubscribeWithSnapshot(ctx, sessionID, buffer)
	return snapshot, sub.Events(), nil
}
