package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func newSessionTestLoadResult() *teamloader.LoadResult {
	agt := agent.New("root", "instructions", agent.WithModel(rootTestProvider{}))
	return &teamloader.LoadResult{Team: team.New(team.WithAgents(agt))}
}

// An explicit --session ID that doesn't exist yet creates a session with that
// exact ID instead of failing, so a caller can own the ID across runs.
func TestCreateLocalRuntimeAndSession_ExplicitUnknownIDCreatesWithThatID(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "board-card-42"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, "board-card-42", sess.ID)

	// Not persisted yet: creation stays lazy until first content.
	_, err = store.GetSession(t.Context(), "board-card-42")
	require.ErrorIs(t, err, session.ErrNotFound)
}

// An explicit --session ID that already exists resumes that session.
func TestCreateLocalRuntimeAndSession_ExplicitExistingIDResumes(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, "existing", sess.ID)
}

// Resuming with --yolo a session created without it backfills
// SafetyPolicy=autonomous (issue #3479), matching fresh --yolo sessions.
func TestCreateLocalRuntimeAndSession_ResumeWithYoloBackfillsSafetyPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing", ToolsApproved: true}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.True(t, sess.ToolsApproved)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
}

// An explicit stored SafetyPolicy survives a plain resume (no flags):
// the mode is session-scoped state.
func TestCreateLocalRuntimeAndSession_ResumeKeepsExplicitSafetyPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyBalanced, sess.SafetyPolicy)
}

// A plain resume (no --yolo, no --safety) of a session stored as
// autonomous must leave the stored state untouched: the policy stays
// autonomous AND the legacy ToolsApproved flag stays true, so the two
// fields remain in sync for legacy readers.
func TestCreateLocalRuntimeAndSession_PlainResumeKeepsAutonomousInSync(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyAutonomous),
	)))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.True(t, sess.ToolsApproved, "plain resume must not reset the legacy flag out from under an autonomous policy")
}

// A plain resume of a legacy stored session — raw ToolsApproved=true with
// no SafetyPolicy, the shape old stores deserialize to — must leave that
// raw shape untouched: the blanket approval stays effective and no policy
// is written onto the session behind the user's back.
func TestCreateLocalRuntimeAndSession_PlainResumeKeepsLegacyToolsApprovedShape(t *testing.T) {
	t.Parallel()

	// WithToolsApproved(true) backfills SafetyPolicy=autonomous, so build
	// the legacy deserialized shape via raw field writes instead.
	legacy := session.New(session.WithID("existing"))
	legacy.ToolsApproved = true
	legacy.SafetyPolicy = ""

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), legacy))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.True(t, sess.ToolsApproved, "plain resume must keep the legacy blanket approval")
	assert.Equal(t, session.SafetyPolicy(""), sess.SafetyPolicy, "plain resume must not invent an explicit policy on a legacy session")
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy(), "the legacy flag alone must stay effective as autonomous")
}

// --yolo on resume escalates even a session stored with an explicit
// non-autonomous mode: the flag is a direct user instruction for THIS
// run, not a default that only fills a gap.
func TestCreateLocalRuntimeAndSession_ResumeWithYoloEscalatesStoredPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing", ToolsApproved: true}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.True(t, sess.ToolsApproved)
}

// When both --safety and --yolo are given, the more explicit --safety
// wins — same precedence as buildSessionOpts applies to new sessions.
func TestCreateLocalRuntimeAndSession_ResumeSafetyFlagWinsOverYolo(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{
		AgentName:       "root",
		ResumeSessionID: "existing",
		ToolsApproved:   true,
		SafetyPolicy:    session.SafetyPolicyStrict,
	}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, sess.SafetyPolicy)
	assert.False(t, sess.ToolsApproved, "explicit strict must revoke the blanket approval")
}

// A relative ref (e.g. -1) is resume-only: it must resolve against existing
// sessions and never creates one.
func TestCreateLocalRuntimeAndSession_RelativeRefDoesNotCreate(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "-1"}

	_, _, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.Error(t, err)
}
