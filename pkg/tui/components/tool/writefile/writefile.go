package writefile

import (
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func New(runtime *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBase(runtime, msg, sessionState, toolcommon.SimpleRenderer(
		toolcommon.ExtractField(func(a filesystem.WriteFileArgs) string { return a.Path }),
	))
}
