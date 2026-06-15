package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestParentIdleClearsTabRunningIndicator(t *testing.T) {
	s := newTestSupervisor([]string{"root"}, "root")
	runner := s.runners["root"]
	runner.IsRunning = true

	s.handleRuntimeEvent("root", &runtime.ParentIdleEvent{SessionID: "root", Count: 1, IDs: []string{"child"}})

	assert.False(t, runner.IsRunning, "ParentIdle means the parent tab is parked, not actively working")
}
