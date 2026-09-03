// Package toolsets wires every built-in toolset type into a
// teamloader.ToolsetRegistry. Importing it links all of them; embedders that
// want a smaller binary build their own registry from the individual
// packages' Creator functions instead.
package toolsets

import (
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools/a2a"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/api"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
	"github.com/docker/docker-agent/pkg/tools/builtin/environment"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	filetool "github.com/docker/docker-agent/pkg/tools/builtin/file"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	gittool "github.com/docker/docker-agent/pkg/tools/builtin/git"
	"github.com/docker/docker-agent/pkg/tools/builtin/lsp"
	"github.com/docker/docker-agent/pkg/tools/builtin/mcpcatalog"
	"github.com/docker/docker-agent/pkg/tools/builtin/memory"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/openapi"
	"github.com/docker/docker-agent/pkg/tools/builtin/openurl"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/rag"
	"github.com/docker/docker-agent/pkg/tools/builtin/scheduler"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessioncontext"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/tasks"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
	"github.com/docker/docker-agent/pkg/tools/builtin/webhook"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

func NewDefaultToolsetRegistry() teamloader.ToolsetRegistry {
	return teamloader.NewToolsetRegistry(DefaultToolsetCreators())
}

// DefaultToolsetCreators maps every built-in toolset type to its creator.
// Callers may copy and trim it before building a registry.
func DefaultToolsetCreators() map[string]teamloader.ToolsetCreator {
	return map[string]teamloader.ToolsetCreator{
		"a2a":               a2a.Creator,
		"api":               api.Creator,
		"background_agents": agenttool.Creator,
		"background_jobs":   backgroundjobs.Creator,
		"environment":       environment.Creator,
		"fetch":             fetch.Creator,
		"file":              filetool.Creator,
		"filesystem":        filesystem.Creator,
		"git":               gittool.Creator,
		"lsp":               lsp.Creator,
		"mcp":               mcp.Creator,
		"mcp_catalog":       mcpcatalog.Creator,
		"memory":            memory.Creator,
		"model_picker":      modelpicker.Creator,
		"open_url":          openurl.Creator,
		"openapi":           openapi.Creator,
		"plan":              plan.Creator,
		"rag":               rag.Creator,
		"scheduler":         scheduler.Creator,
		"script":            shell.ScriptCreator,
		"session_context":   sessioncontext.Creator,
		"session_plan":      sessionplan.Creator,
		"shell":             shell.Creator,
		"tasks":             tasks.Creator,
		"think":             think.Creator,
		"todo":              todo.Creator,
		"user_prompt":       userprompt.Creator,
		"webhook":           webhook.Creator,
	}
}
