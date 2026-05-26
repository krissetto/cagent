package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

const publicRuntimeEventScopeSession = "session"

// PublicRuntimeEventStore is the runtime-facing durable public event API.
// It aliases the neutral session package interface to keep storage ownership
// in pkg/session and avoid package cycles.
type PublicRuntimeEventStore = session.PublicRuntimeEventStore

type publicEventObserver struct {
	store session.PublicRuntimeEventStore
}

func newPublicEventObserver(store session.Store) EventObserver {
	publicStore, ok := store.(session.PublicRuntimeEventStore)
	if !ok || publicStore == nil {
		return nil
	}
	return &publicEventObserver{store: publicStore}
}

func (o *publicEventObserver) OnRunStart(context.Context, *session.Session) {}

func (o *publicEventObserver) OnEvent(ctx context.Context, sess *session.Session, event Event) {
	if _, internal := event.(*MessageAddedEvent); internal {
		return
	}
	if event == nil || sess == nil {
		return
	}

	sessionID := sess.ID
	if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != "" {
		sessionID = scoped.GetSessionID()
	}
	if sessionID == "" {
		return
	}
	rootID := sess.EffectiveRootID()
	if rootID == "" {
		rootID = sessionID
	}

	payload, err := json.Marshal(event)
	if err != nil {
		slog.WarnContext(ctx, "skipping public runtime event with non-json payload", "type", publicEventType(event), "error", err)
		return
	}

	_, err = o.store.AppendPublicRuntimeEvent(ctx, session.PublicRuntimeEvent{
		SessionID:   sessionID,
		RootID:      rootID,
		Scope:       publicRuntimeEventScopeSession,
		Type:        publicEventType(event),
		PayloadJSON: string(payload),
	})
	if err != nil {
		slog.WarnContext(ctx, "failed to append public runtime event", "session_id", sessionID, "root_id", rootID, "type", publicEventType(event), "error", err)
	}
}

// ReplayPublicRuntimeEvents replays durable public runtime events from store.
func ReplayPublicRuntimeEvents(ctx context.Context, store session.Store, query session.PublicRuntimeEventQuery) ([]session.PublicRuntimeEvent, error) {
	publicStore, ok := store.(session.PublicRuntimeEventStore)
	if !ok || publicStore == nil {
		return nil, nil
	}
	return publicStore.ReplayPublicRuntimeEvents(ctx, query)
}

func publicEventType(event Event) string {
	if event == nil {
		return ""
	}
	v := reflect.Indirect(reflect.ValueOf(event))
	if v.IsValid() && v.Kind() == reflect.Struct {
		field := v.FieldByName("Type")
		if field.IsValid() && field.Kind() == reflect.String && field.String() != "" {
			return field.String()
		}
	}
	t := reflect.TypeOf(event)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	name = strings.TrimSuffix(name, "Event")
	return camelToSnake(name)
}

func camelToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
