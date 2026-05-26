package session

import (
	"context"
	"time"
)

// PublicRuntimeEvent is the durable, append-only public runtime event shape.
// It is intentionally neutral storage owned by pkg/session so both the runtime
// and clients can append/replay without introducing package cycles.
type PublicRuntimeEvent struct {
	EventID     int64
	SessionID   string
	RootID      string
	Scope       string
	Type        string
	PayloadJSON string
	CreatedAt   time.Time
}

// PublicRuntimeEventQuery scopes durable public runtime event replay.
// AfterEventID is an exclusive cursor: replay returns events with
// EventID > AfterEventID.
type PublicRuntimeEventQuery struct {
	SessionID    string
	RootID       string
	AfterEventID int64
	Limit        int
}

// PublicRuntimeEventStore is implemented by stores that support the durable
// public runtime event stream.
type PublicRuntimeEventStore interface {
	AppendPublicRuntimeEvent(ctx context.Context, event PublicRuntimeEvent) (PublicRuntimeEvent, error)
	ReplayPublicRuntimeEvents(ctx context.Context, query PublicRuntimeEventQuery) ([]PublicRuntimeEvent, error)
}
