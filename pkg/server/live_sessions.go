package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type liveSessionRuntime interface {
	runtime.LiveSessionRuntime
}

type liveRuntimeMatch struct {
	runtime liveSessionRuntime
	rs      *activeRuntimes
	session *session.Session
	root    bool
}

func (sm *SessionManager) liveRuntimeForSession(sessionID string) (*liveRuntimeMatch, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	var match *liveRuntimeMatch
	var firstErr error
	sm.runtimeSessions.Range(func(rootID string, rs *activeRuntimes) bool {
		lrt, ok := rs.runtime.(liveSessionRuntime)
		if !ok {
			return true
		}
		if rootID == sessionID || strings.HasPrefix(rootID, sessionID) {
			if match != nil {
				firstErr = errors.New("ambiguous session id")
				return false
			}
			match = &liveRuntimeMatch{runtime: lrt, rs: rs, session: rs.session, root: true}
			return true
		}
		if child, ok := lrt.LiveChildSession(sessionID); ok {
			if match != nil {
				firstErr = errors.New("ambiguous session id")
				return false
			}
			match = &liveRuntimeMatch{runtime: lrt, rs: rs, session: child}
		}
		return true
	})
	if firstErr != nil {
		return nil, firstErr
	}
	if match == nil {
		return nil, ErrSessionNotRunning
	}
	return match, nil
}

func (sm *SessionManager) LiveSessionTree(ctx context.Context, sessionID string) (*runtime.LiveSessionTree, error) {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return nil, err
	}
	return match.runtime.LiveSessionTree(ctx, sessionID)
}

func (sm *SessionManager) SteerLiveSession(_ context.Context, sessionID string, messages []api.Message) error {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		if err := match.runtime.SteerSessionByID(sessionID, runtime.QueuedMessage{Content: msg.Content, MultiContent: msg.MultiContent}); err != nil {
			return err
		}
	}
	return nil
}

func (sm *SessionManager) FollowUpLiveSession(_ context.Context, sessionID string, messages []api.Message) (bool, error) {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return false, err
	}
	for _, msg := range messages {
		if err := match.runtime.FollowUpSessionByID(sessionID, runtime.QueuedMessage{Content: msg.Content, MultiContent: msg.MultiContent}); err != nil {
			return false, err
		}
	}
	streaming := !match.rs.streaming.TryLock()
	if !streaming {
		match.rs.streaming.Unlock()
	}
	return streaming, nil
}

func (sm *SessionManager) CloseLiveSession(_ context.Context, sessionID string) error {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return err
	}
	return match.runtime.CloseSessionByID(sessionID)
}

func (sm *SessionManager) InterruptLiveSession(_ context.Context, sessionID string) error {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return err
	}
	return match.runtime.InterruptSessionByID(sessionID)
}

func (sm *SessionManager) StopLiveSession(_ context.Context, sessionID string) error {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return err
	}
	return match.runtime.StopSessionByID(sessionID)
}

func (sm *SessionManager) liveEventSource(sessionID string) (runtime.LiveEventSourceWithSnapshot, error) {
	match, err := sm.liveRuntimeForSession(sessionID)
	if err != nil {
		return nil, err
	}
	return match.runtime, nil
}

func (s *Server) getLiveSessionTree(c echo.Context) error {
	tree, err := s.sm.LiveSessionTree(c.Request().Context(), c.Param("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionNotRunning), errors.Is(err, runtime.ErrLiveSessionUnavailable):
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	return c.JSON(http.StatusOK, tree)
}

func (s *Server) closeLiveSession(c echo.Context) error {
	if err := s.sm.CloseLiveSession(c.Request().Context(), c.Param("id")); err != nil {
		return liveSessionControlError("failed to close live session", err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"status": "closing"})
}

func (s *Server) interruptLiveSession(c echo.Context) error {
	if err := s.sm.InterruptLiveSession(c.Request().Context(), c.Param("id")); err != nil {
		return liveSessionControlError("failed to interrupt live session", err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"status": "interrupting"})
}

func (s *Server) stopLiveSession(c echo.Context) error {
	if err := s.sm.StopLiveSession(c.Request().Context(), c.Param("id")); err != nil {
		return liveSessionControlError("failed to stop live session", err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"status": "stopping"})
}

func liveSessionControlError(prefix string, err error) error {
	status := http.StatusConflict
	if errors.Is(err, ErrSessionNotRunning) || errors.Is(err, runtime.ErrLiveSessionUnavailable) {
		status = http.StatusNotFound
	}
	return echo.NewHTTPError(status, fmt.Sprintf("%s: %v", prefix, err))
}
