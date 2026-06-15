package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/subagents"
)

func TestSQLiteReloadedSubagentSessionKeepsDirectTranscript(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	store, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)

	workerProvider := &recordingSubagentProvider{}
	worker := agent.New("worker", "worker", agent.WithModel(workerProvider))
	subagentTools, err := subagents.New(nil).Tools(ctx)
	require.NoError(t, err)
	root := agent.New("root", "root", agent.WithModel(&recordingSubagentProvider{}), agent.WithTools(subagentTools...), agent.WithSubAgents(worker), agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "worker", Agent: "worker"}))
	parent := session.New(session.WithID("parent-session"), session.WithAgentName("root"), session.WithToolsApproved(true))

	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, worker)), WithModelStore(mockModelStore{}), WithSessionCompaction(false), WithSessionStore(store))
	require.NoError(t, err)
	defer func() { _ = rt.Close() }()
	rt.ensureSessionPersisted(ctx, parent)

	agentTools, err := root.Tools(ctx)
	require.NoError(t, err)
	var childID string
	childStarted := make(chan struct{}, 1)
	rt.processToolCalls(ctx, parent, []tools.ToolCall{{
		ID:       "call-subagent",
		Type:     "function",
		Function: tools.FunctionCall{Name: "subagent_start", Arguments: `{"agent":"worker","task":"durable child prompt"}`},
	}}, agentTools, EventSinkFunc(func(ev Event) {
		if e, ok := ev.(*SubAgentStartedEvent); ok {
			childID = e.SubAgent.ID
			childStarted <- struct{}{}
		}
	}))
	require.NotEmpty(t, childID)
	<-childStarted

	require.Eventually(t, func() bool {
		child, err := store.GetSession(ctx, childID)
		if err != nil {
			return false
		}
		return transcriptContains(child, "durable child prompt") && transcriptContainsPrefix(child, "child reply ")
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, rt.subagents.Stop(parent, childID))
	require.NoError(t, rt.Close())

	reloaded, err := session.NewSQLiteSessionStore(dbPath)
	require.NoError(t, err)
	child, err := reloaded.GetSession(ctx, childID)
	require.NoError(t, err)
	require.True(t, transcriptContains(child, "durable child prompt"), "reloaded child transcript missing user prompt: %#v", child.Messages)
	require.True(t, transcriptContainsPrefix(child, "child reply "), "reloaded child transcript missing assistant reply: %#v", child.Messages)
}

func transcriptContainsPrefix(sess *session.Session, prefix string) bool {
	if sess == nil {
		return false
	}
	for _, item := range sess.GetAllMessages() {
		if strings.HasPrefix(item.Message.Content, prefix) {
			return true
		}
	}
	return false
}

func transcriptContains(sess *session.Session, text string) bool {
	if sess == nil {
		return false
	}
	for _, item := range sess.GetAllMessages() {
		if item.Message.Content == text {
			return true
		}
	}
	return false
}
