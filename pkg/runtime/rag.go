package runtime

import (
	"fmt"
	"log/slog"

	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools/builtin"
)

// ragEventForwarder returns a callback that converts RAG manager events to runtime events.
// agentName is the session-resolved agent name captured at configuration time so
// the callback does not read the root runtime's mutable CurrentAgentName().
func ragEventForwarder(ragName, agentName string, sendEvent func(Event)) builtin.RAGEventCallback {
	return func(ragEvent ragtypes.Event) {
		slog.Debug("Forwarding RAG event", "type", ragEvent.Type, "rag", ragName, "agent", agentName)

		switch ragEvent.Type {
		case ragtypes.EventTypeIndexingStarted:
			sendEvent(RAGIndexingStarted(ragName, ragEvent.StrategyName))
		case ragtypes.EventTypeIndexingProgress:
			if ragEvent.Progress != nil {
				sendEvent(RAGIndexingProgress(ragName, ragEvent.StrategyName, ragEvent.Progress.Current, ragEvent.Progress.Total, agentName))
			}
		case ragtypes.EventTypeIndexingComplete:
			sendEvent(RAGIndexingCompleted(ragName, ragEvent.StrategyName))
		case ragtypes.EventTypeUsage:
			sendEvent(NewTokenUsageEvent("", agentName, &Usage{
				InputTokens:   ragEvent.TotalTokens,
				ContextLength: ragEvent.TotalTokens,
				Cost:          ragEvent.Cost,
			}))
		case ragtypes.EventTypeError:
			if ragEvent.Error != nil {
				sendEvent(Error(fmt.Sprintf("RAG %s error: %v", ragName, ragEvent.Error)))
			}
		default:
			slog.Debug("Unhandled RAG event type", "type", ragEvent.Type, "rag", ragName)
		}
	}
}
