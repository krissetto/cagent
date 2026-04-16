package messages

import (
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// TestModel wraps the messages model to provide test-only inspection methods.
// Use NewTestModel in tests that need to inspect the internal messages state.
type TestModel struct {
	*model
}

// NewTestModel creates a TestModel for use in tests.
// It satisfies the Model interface and also exposes GetMessages for assertions.
func NewTestModel(sessionState *service.SessionState) *TestModel {
	return &TestModel{model: newModel(120, 24, sessionState)}
}

// GetMessages returns a snapshot of the current messages for test assertions.
func (t *TestModel) GetMessages() []*types.Message {
	msgs := make([]*types.Message, len(t.model.messages))
	copy(msgs, t.model.messages)
	return msgs
}
