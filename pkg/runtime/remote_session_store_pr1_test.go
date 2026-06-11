package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestRemoteSessionStoreOptionalCapabilitiesUnsupported(t *testing.T) {
	store := NewRemoteSessionStore(&stubRemoteClient{})

	_, err := store.AddMessageAt(t.Context(), "session", 0, session.UserMessage("hi"))
	require.ErrorIs(t, err, ErrUnsupported)

	_, err = store.GetChildSessions(t.Context(), "session")
	require.ErrorIs(t, err, ErrUnsupported)

	_, err = store.GetSessionTree(t.Context(), "root")
	require.ErrorIs(t, err, ErrUnsupported)

	_, err = store.ResolveRootID(t.Context(), "session")
	require.ErrorIs(t, err, ErrUnsupported)
}
