package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/runtime"
)

func (sm *SessionManager) LiveSessionTree(ctx context.Context, sessionID string) (*runtime.LiveSessionTree, error) {
	sessionRuntime, ok := sm.runtimeSessions.Load(sessionID)
	if !ok {
		return nil, ErrSessionNotRunning
	}
	treeRuntime, ok := sessionRuntime.runtime.(interface {
		LiveSessionTree(ctx context.Context, sessionID string) (*runtime.LiveSessionTree, error)
	})
	if !ok {
		return nil, runtime.ErrLiveSessionUnavailable
	}
	return treeRuntime.LiveSessionTree(ctx, sessionID)
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
