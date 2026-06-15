package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestNewAttachedHistoryOnlyIsReadOnlyAndDoesNotAttachLiveTail(t *testing.T) {
	ctx := t.Context()
	stored := session.New(session.WithID("child"), session.WithTitle("Stored Child Title"))
	rt := &attachedLiveRuntime{mockRuntime: &mockRuntime{}}

	attached := NewAttached(ctx, rt, stored, runtime.LiveSessionNode{ID: "child", AgentName: "greppy", Live: false})

	require.True(t, attached.IsReadOnly())
	require.ErrorContains(t, attached.FollowUpWithAttachments("nope", nil), "follow-up")
	require.Empty(t, rt.followID)
}

func TestNewAttachedClosedLiveNodeIsReadOnly(t *testing.T) {
	ctx := t.Context()
	stored := session.New(session.WithID("child"))
	rt := &attachedLiveRuntime{mockRuntime: &mockRuntime{}}

	attached := NewAttached(ctx, rt, stored, runtime.LiveSessionNode{ID: "child", AgentName: "greppy", Live: true, Status: "finalized"})

	require.True(t, attached.IsReadOnly())
	require.ErrorContains(t, attached.FollowUpWithAttachments("nope", nil), "follow-up")
	require.Empty(t, rt.followID)
}
