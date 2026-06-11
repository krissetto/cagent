package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type liveTreeRuntime struct {
	fakeRuntime

	tree *runtime.LiveSessionTree
}

func (r *liveTreeRuntime) LiveSessionTree(context.Context, string) (*runtime.LiveSessionTree, error) {
	return r.tree, nil
}

func TestServerGetLiveSessionTreeEndpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New(session.WithID("root"))
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := &SessionManager{
		runtimeSessions: concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions: concurrent.NewMap[string, *activeRuntimes](),
		sessionStore:    store,
		Sources:         config.Sources{},
		runConfig:       &config.RuntimeConfig{},
		sessionReady:    make(chan struct{}),
	}
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{
		runtime: &liveTreeRuntime{tree: &runtime.LiveSessionTree{Root: &runtime.LiveSessionNode{
			ID:      "root",
			Title:   "Root title",
			Depth:   0,
			Preview: "root preview",
			Children: []*runtime.LiveSessionNode{{
				ID:          "child",
				ParentID:    "root",
				AgentName:   "reviewer",
				Title:       "Child title",
				Depth:       1,
				Preview:     "child preview",
				LastPreview: "child preview",
				Live:        true,
			}},
		}}},
		session: sess,
	})

	srv := NewWithManager(sm, "")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/sessions/root/tree", http.NoBody)
	rec := httptest.NewRecorder()
	srv.e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var tree runtime.LiveSessionTree
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tree))
	require.NotNil(t, tree.Root)
	assert.Equal(t, "Root title", tree.Root.Title)
	assert.Equal(t, "root preview", tree.Root.Preview)
	require.Len(t, tree.Root.Children, 1)
	assert.Equal(t, "child", tree.Root.Children[0].ID)
	assert.Equal(t, 1, tree.Root.Children[0].Depth)
	assert.True(t, tree.Root.Children[0].Live)

	missingReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/sessions/missing/tree", http.NoBody)
	missingRec := httptest.NewRecorder()
	srv.e.ServeHTTP(missingRec, missingReq)
	assert.Equal(t, http.StatusNotFound, missingRec.Code)
}
