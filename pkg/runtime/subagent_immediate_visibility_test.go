package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/subagents"
)

func TestSubagentStartPublishesParentVisibleTreeBeforeParentToolResult(t *testing.T) {
	ctx := t.Context()

	workerModel := &blockingToolProvider{
		first: newStreamBuilder().AddContent("child reply").AddStopWithUsage(1, 1).Build(),
	}
	worker := agent.New("worker", "worker", agent.WithModel(workerModel))
	root := agent.New("root", "root", agent.WithModel(&blockingToolProvider{}), agent.WithToolSets(subagents.New(nil)), agent.WithSubAgents(worker), agent.WithSubAgentSpecs(agent.SubAgentSpec{Name: "worker", Agent: "worker"}))
	parent := session.New(session.WithID("parent-session"), session.WithAgentName("root"), session.WithUserMessage("start worker"), session.WithToolsApproved(true))
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(ctx, parent))
	rt, err := NewLocalRuntime(team.New(team.WithAgents(root, worker)), WithModelStore(mockModelStore{}), WithSessionCompaction(false), WithSessionStore(store))
	require.NoError(t, err)

	// Runtime-managed tool handlers should emit lifecycle/tree events onto the
	// currently executing parent stream before the tool response event is recorded.
	agentTools, err := root.Tools(ctx)
	require.NoError(t, err)
	emitted := make([]Event, 0, 8)
	rt.processToolCalls(ctx, parent, []tools.ToolCall{{
		ID:       "call-subagent",
		Type:     "function",
		Function: tools.FunctionCall{Name: "subagent_start", Arguments: `{"agent":"worker","task":"do child work"}`},
	}}, agentTools, EventSinkFunc(func(ev Event) { emitted = append(emitted, ev) }))

	var childID string
	seenStarted := false
	seenTreeWithChild := false
	seenToolResult := false
	require.NotEmpty(t, emitted, "expected tool events")
	for _, ev := range emitted {
		switch e := ev.(type) {
		case *SubAgentStartedEvent:
			seenStarted = true
			childID = e.SubAgent.ID
		case *LiveSessionTreeChangedEvent:
			if liveSessionTreeHasChild(e.Tree, "parent-session") {
				seenTreeWithChild = true
			}
		case *ToolCallConfirmationEvent:
			t.Fatalf("subagent_start unexpectedly asked for confirmation: %#v", e)
		case *ToolCallResponseEvent:
			if e.ToolDefinition.Name == "subagent_start" {
				seenToolResult = true
			}
		}
		if seenToolResult {
			break
		}
	}

	require.True(t, seenStarted, "root stream must receive SubAgentStarted during tool execution")
	require.True(t, seenTreeWithChild, "root stream must receive a tree containing the child before tool result")
	require.NotEmpty(t, childID)

	require.NoError(t, rt.subagents.Stop(parent, childID))
}

func liveSessionTreeHasChild(tree *LiveSessionTree, parentID string) bool {
	if tree == nil || tree.Root == nil {
		return false
	}
	if tree.Root.ID == parentID && len(tree.Root.Children) > 0 && tree.Root.Children[0].ID != "" {
		return true
	}
	for _, child := range tree.Root.Children {
		if child.ID == parentID && len(child.Children) > 0 && child.Children[0].ID != "" {
			return true
		}
	}
	return false
}

type blockingToolProvider struct {
	first       chat.MessageStream
	second      chat.MessageStream
	secondReady chan struct{}
	calls       int
}

func (p *blockingToolProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/blocking") }

func (p *blockingToolProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.calls++
	if p.calls == 1 {
		return p.first, nil
	}
	if p.secondReady != nil {
		<-p.secondReady
	}
	return p.second, nil
}

func (p *blockingToolProvider) BaseConfig() base.Config { return base.Config{} }

func (p *blockingToolProvider) MaxTokens() int { return 0 }
