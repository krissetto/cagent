// File live_sessions.go implements the server side of PR5: live-session tree
// observability and control endpoints.
//
// Now that PR4 has landed, we type-assert runtime.Runtime to
// runtime.LiveSessionRuntime and use PR4's exported interfaces (LiveSessionNode,
// SessionTree, AttachLiveSession with cancel-func, SteerSessionByID, etc.)
// directly. If the runtime does not implement LiveSessionRuntime, handlers
// degrade gracefully with HTTP 503.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
)

// ---------------------------------------------------------------------------
// resolveLiveSession
// ---------------------------------------------------------------------------

// resolveLiveSession finds the [runtime.LiveSessionRuntime] that owns the
// given sessionID, walking root sessions first (O(1)) then scanning all
// runtimes for a descendant match (O(n runtimes)).
func (sm *SessionManager) resolveLiveSession(sessionID string) (runtime.LiveSessionRuntime, error) {
	// Root session fast path.
	if art, ok := sm.runtimeSessions.Load(sessionID); ok {
		lsr, supports := art.runtime.(runtime.LiveSessionRuntime)
		if !supports {
			return nil, errors.New("runtime does not support live sessions")
		}
		return lsr, nil
	}

	// Descendant search: ask each root runtime's SessionTree whether it
	// contains the session id.
	var found runtime.LiveSessionRuntime
	sm.runtimeSessions.Range(func(_ string, art *activeRuntimes) bool {
		lsr, ok := art.runtime.(runtime.LiveSessionRuntime)
		if !ok {
			return true // continue
		}
		tree, err := lsr.LiveSessionTree(sessionID)
		if err != nil {
			return true // this runtime doesn't know about it either
		}
		// Accept this runtime if the tree has any nodes or if the tree
		// root matches the session id.
		if tree != nil {
			if _, exists := tree.Node(sessionID); exists {
				found = lsr
				return false // stop
			}
		}
		return true
	})
	if found != nil {
		return found, nil
	}
	return nil, fmt.Errorf("live session %s not found", sessionID)
}

// ---------------------------------------------------------------------------
// SessionManager helpers (used by handlers)
// ---------------------------------------------------------------------------

// LiveSessionTree returns a flat, DFS-ordered slice of live nodes rooted
// at rootSessionID by delegating to PR4's SessionTree.Slice().
func (sm *SessionManager) LiveSessionTree(rootSessionID string) ([]api.LiveSessionNode, error) {
	art, ok := sm.runtimeSessions.Load(rootSessionID)
	if !ok {
		return nil, fmt.Errorf("session %s not found or not running", rootSessionID)
	}
	lsr, ok := art.runtime.(runtime.LiveSessionRuntime)
	if !ok {
		return nil, errors.New("runtime does not support live sessions")
	}
	tree, err := lsr.LiveSessionTree(rootSessionID)
	if err != nil {
		return nil, err
	}
	return runtimeNodesToAPI(tree.Slice()), nil
}

// GetLiveSession returns the single live-node for an arbitrary session id.
func (sm *SessionManager) GetLiveSession(ctx context.Context, sessionID string) (api.LiveSessionNode, error) {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return api.LiveSessionNode{}, err
	}
	tree, err := lsr.LiveSessionTree(sessionID)
	if err != nil {
		return api.LiveSessionNode{}, err
	}
	node, ok := tree.Node(sessionID)
	if !ok {
		// Try the root of whatever tree we got back.
		node = tree.Root()
	}
	return runtimeNodeToAPI(node), nil
}

// GetLiveSessionSnapshot returns the serialised event history via
// AttachLiveSessionWithSnapshot (PR4 full-session snapshot).
func (sm *SessionManager) GetLiveSessionSnapshot(ctx context.Context, sessionID string) ([]json.RawMessage, error) {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return nil, err
	}
	src, ok := lsr.(runtime.LiveEventSourceWithSnapshot)
	if !ok {
		return []json.RawMessage{}, nil
	}
	events, _, cancel, err := src.AttachLiveSessionWithSnapshot(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	cancel()
	raw := make([]json.RawMessage, 0, len(events))
	for _, ev := range events {
		b, marshalErr := json.Marshal(ev)
		if marshalErr != nil {
			continue
		}
		raw = append(raw, b)
	}
	return raw, nil
}

// AttachLiveSession opens a live event subscription for an arbitrary session.
func (sm *SessionManager) AttachLiveSession(ctx context.Context, sessionID string) (<-chan runtime.Event, func(), error) {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return nil, nil, err
	}
	return lsr.AttachLiveSession(ctx, sessionID)
}

// SteerLiveSession injects messages into an arbitrary live session.
func (sm *SessionManager) SteerLiveSession(_ context.Context, sessionID string, messages []api.Message) error {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		if err := lsr.SteerSessionByID(sessionID, runtime.QueuedMessage{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
		}); err != nil {
			return err
		}
	}
	return nil
}

// FollowUpLiveSession queues messages for an arbitrary live session.
func (sm *SessionManager) FollowUpLiveSession(_ context.Context, sessionID string, messages []api.Message) error {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		if err := lsr.FollowUpSessionByID(sessionID, runtime.QueuedMessage{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
		}); err != nil {
			return err
		}
	}
	return nil
}

// CloseLiveSession asks a live session to close cleanly.
func (sm *SessionManager) CloseLiveSession(_ context.Context, sessionID string) error {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	return lsr.CloseSessionByID(sessionID)
}

// InterruptLiveSession cancels the current turn of a live session.
func (sm *SessionManager) InterruptLiveSession(_ context.Context, sessionID string) error {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	return lsr.InterruptSessionByID(sessionID)
}

// StopLiveSession forcibly stops a live session.
func (sm *SessionManager) StopLiveSession(_ context.Context, sessionID string) error {
	lsr, err := sm.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	return lsr.StopSessionByID(sessionID)
}

// ---------------------------------------------------------------------------
// API conversion helpers (runtime.LiveSessionNode → api.LiveSessionNode)
// ---------------------------------------------------------------------------

func runtimeNodeToAPI(n runtime.LiveSessionNode) api.LiveSessionNode {
	return api.LiveSessionNode{
		ID:           n.ID,
		ParentID:     n.ParentID,
		AgentName:    n.AgentName,
		Status:       n.Status,
		Title:        n.Title,
		Depth:        n.Depth,
		LastPreview:  n.LastPreview,
		CreatedAt:    n.CreatedAt,
		LastUpdateAt: n.LastUpdateAt,
	}
}

func runtimeNodesToAPI(nodes []runtime.LiveSessionNode) []api.LiveSessionNode {
	out := make([]api.LiveSessionNode, len(nodes))
	for i, n := range nodes {
		out[i] = runtimeNodeToAPI(n)
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP handlers on *Server
// ---------------------------------------------------------------------------

// getLiveSessionTree handles GET /api/sessions/:id/tree.
func (s *Server) getLiveSessionTree(c echo.Context) error {
	rootID := c.Param("id")
	nodes, err := s.sm.LiveSessionTree(rootID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("live session tree unavailable: %v", err))
	}
	return c.JSON(http.StatusOK, api.LiveSessionTreeResponse{Nodes: nodes})
}

// getLiveSession handles GET /api/live-sessions/:id.
func (s *Server) getLiveSession(c echo.Context) error {
	node, err := s.sm.GetLiveSession(c.Request().Context(), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("live session not found: %v", err))
	}
	return c.JSON(http.StatusOK, api.LiveSessionResponse{
		ID:        node.ID,
		AgentName: node.AgentName,
		Status:    node.Status,
	})
}

// getLiveSessionSnapshot handles GET /api/live-sessions/:id/snapshot.
func (s *Server) getLiveSessionSnapshot(c echo.Context) error {
	sessionID := c.Param("id")
	events, err := s.sm.GetLiveSessionSnapshot(c.Request().Context(), sessionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("live session snapshot unavailable: %v", err))
	}
	if events == nil {
		events = []json.RawMessage{}
	}
	return c.JSON(http.StatusOK, api.LiveSessionSnapshotResponse{
		SessionID: sessionID,
		Events:    events,
	})
}

// attachLiveSession handles GET /api/live-sessions/:id/attach (SSE).
// Streams full snapshot (from PR4 AttachLiveSessionWithSnapshot) then live tail.
func (s *Server) attachLiveSession(c echo.Context) error {
	sessionID := c.Param("id")
	ctx := c.Request().Context()

	// Attempt PR4 full-snapshot attach.
	var snapshot []runtime.Event
	var tail <-chan runtime.Event
	var cancel func()

	lsr, lsrErr := s.sm.resolveLiveSession(sessionID)
	if lsrErr == nil {
		if src, ok := lsr.(runtime.LiveEventSourceWithSnapshot); ok {
			snapshot, tail, cancel, lsrErr = src.AttachLiveSessionWithSnapshot(ctx, sessionID)
		} else {
			// Fallback: live-only attach with no snapshot.
			tail, cancel, lsrErr = lsr.AttachLiveSession(ctx, sessionID)
		}
	}
	if lsrErr != nil {
		return echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("failed to attach live session: %v", lsrErr))
	}
	defer cancel()

	w := c.Response()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeSSE := func(data []byte) bool {
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		if err != nil {
			return false
		}
		w.Flush()
		return true
	}

	// Emit snapshot events.
	for _, ev := range snapshot {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if !writeSSE(b) {
			return nil
		}
	}

	// Stream live events.
	for ev := range tail {
		if ctx.Err() != nil {
			return nil
		}
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if !writeSSE(b) {
			return nil
		}
	}
	return nil
}

// steerLiveSession handles POST /api/live-sessions/:id/steer.
func (s *Server) steerLiveSession(c echo.Context) error {
	var req api.SteerSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("invalid request body: %v", err))
	}
	if len(req.Messages) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "at least one message is required")
	}
	if err := s.sm.SteerLiveSession(c.Request().Context(), c.Param("id"), req.Messages); err != nil {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("failed to steer live session: %v", err))
	}
	return c.JSON(http.StatusAccepted, api.LiveSessionControlResponse{OK: true, Message: "queued"})
}

// followUpLiveSession handles POST /api/live-sessions/:id/followup.
func (s *Server) followUpLiveSession(c echo.Context) error {
	var req api.SteerSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("invalid request body: %v", err))
	}
	if len(req.Messages) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "at least one message is required")
	}
	if err := s.sm.FollowUpLiveSession(c.Request().Context(), c.Param("id"), req.Messages); err != nil {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("failed to enqueue live follow-up: %v", err))
	}
	return c.JSON(http.StatusAccepted, api.LiveSessionControlResponse{OK: true, Message: "queued"})
}

// closeLiveSession handles POST /api/live-sessions/:id/close.
func (s *Server) closeLiveSession(c echo.Context) error {
	if err := s.sm.CloseLiveSession(c.Request().Context(), c.Param("id")); err != nil {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("failed to close live session: %v", err))
	}
	return c.JSON(http.StatusAccepted, api.LiveSessionControlResponse{OK: true, Message: "closing"})
}

// interruptLiveSession handles POST /api/live-sessions/:id/interrupt.
func (s *Server) interruptLiveSession(c echo.Context) error {
	if err := s.sm.InterruptLiveSession(c.Request().Context(), c.Param("id")); err != nil {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("failed to interrupt live session: %v", err))
	}
	return c.JSON(http.StatusAccepted, api.LiveSessionControlResponse{OK: true, Message: "interrupting"})
}

// stopLiveSession handles POST /api/live-sessions/:id/stop.
func (s *Server) stopLiveSession(c echo.Context) error {
	if err := s.sm.StopLiveSession(c.Request().Context(), c.Param("id")); err != nil {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("failed to stop live session: %v", err))
	}
	return c.JSON(http.StatusAccepted, api.LiveSessionControlResponse{OK: true, Message: "stopping"})
}
