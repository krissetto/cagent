package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestShortRef(t *testing.T) {
	assert.Equal(t, "abc", ShortRef("abc"))
	assert.Equal(t, "abcde", ShortRef("abcde"))
	assert.Equal(t, "abcde", ShortRef("abcdef"))
}

func TestManagerResolveChildRef_ExactAndPrefix(t *testing.T) {
	root := session.New()
	mgr := NewManager(fakeRunner{})

	child := session.New(session.WithAgentName("worker"))
	h, err := mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "a"}, child)
	require.NoError(t, err)

	id, err := mgr.ResolveChildRef(root.ID, h.ID())
	require.NoError(t, err)
	assert.Equal(t, h.ID(), id)

	id, err = mgr.ResolveChildRef(root.ID, ShortRef(h.ID()))
	require.NoError(t, err)
	assert.Equal(t, h.ID(), id)
}

func TestManagerResolveChildRef_NotFound(t *testing.T) {
	root := session.New()
	mgr := NewManager(fakeRunner{})

	_, err := mgr.ResolveChildRef(root.ID, "abcde")
	var nf *NotFoundError
	assert.ErrorAs(t, err, &nf)
}

func TestManagerResolveChildRef_Ambiguous(t *testing.T) {
	root := &session.Session{ID: "root"}
	mgr := NewManager(fakeRunner{})

	// Use synthetic child ids with a shared 5-char prefix so we can exercise the
	// ambiguity path deterministically.
	child1 := &session.Session{ID: "abcde-111", AgentName: "worker"}
	child2 := &session.Session{ID: "abcde-222", AgentName: "worker"}
	_, err := mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "a"}, child1)
	require.NoError(t, err)
	_, err = mgr.StartChild(t.Context(), StartConfig{Parent: root, AgentName: "worker", Task: "b"}, child2)
	require.NoError(t, err)

	_, err = mgr.ResolveChildRef(root.ID, "abcde")
	var amb *AmbiguousRefError
	require.ErrorAs(t, err, &amb)
	assert.Equal(t, "abcde", amb.Ref)
	assert.Equal(t, []string{"abcde-1", "abcde-2"}, amb.Candidates)
}
