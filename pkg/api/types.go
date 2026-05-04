package api

import (
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

type Message struct {
	Role         chat.MessageRole   `json:"role"`
	Content      string             `json:"content"`
	MultiContent []chat.MessagePart `json:"multi_content,omitempty"`
}

// Agent represents an agent in the API
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Multi       bool   `json:"multi"`
}

// CreateAgentRequest represents a request to create an agent
type CreateAgentRequest struct {
	Prompt string `json:"prompt"`
}

// CreateAgentResponse represents the response from creating an agent
type CreateAgentResponse struct {
	Path string `json:"path"`
	Out  string `json:"out"`
}

// CreateAgentConfigRequest represents a request to create an agent manually
type CreateAgentConfigRequest struct {
	Filename    string `json:"filename"`
	Model       string `json:"model"`
	Description string `json:"description"`
	Instruction string `json:"instruction"`
}

// CreateAgentConfigResponse represents the response from creating an agent config
type CreateAgentConfigResponse struct {
	Filepath string `json:"filepath"`
}

// EditAgentConfigRequest represents a request to edit an agent config
type EditAgentConfigRequest struct {
	AgentConfig latest.Config `json:"agent_config"`
	Filename    string        `json:"filename"`
}

// EditAgentConfigResponse represents the response from editing an agent config
type EditAgentConfigResponse struct {
	Message string `json:"message"`
	Path    string `json:"path"`
	Config  any    `json:"config"`
}

// ImportAgentRequest represents a request to import an agent
type ImportAgentRequest struct {
	FilePath string `json:"file_path"`
}

// ImportAgentResponse represents the response from importing an agent
type ImportAgentResponse struct {
	OriginalPath string `json:"originalPath"`
	TargetPath   string `json:"targetPath"`
	Description  string `json:"description"`
}

// ExportAgentsResponse represents the response from exporting agents
type ExportAgentsResponse struct {
	ZipPath      string `json:"zipPath"`
	ZipFile      string `json:"zipFile"`
	ZipDirectory string `json:"zipDirectory"`
	AgentsDir    string `json:"agentsDir"`
	CreatedAt    string `json:"createdAt"`
}

// PullAgentRequest represents a request to pull an agent
type PullAgentRequest struct {
	Name string `json:"name"`
}

// PullAgentResponse represents the response from pulling an agent
type PullAgentResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PushAgentRequest represents a request to push an agent
type PushAgentRequest struct {
	Filepath string `json:"filepath"`
	Tag      string `json:"tag"`
}

// PushAgentResponse represents the response from pushing an agent
type PushAgentResponse struct {
	Filepath string `json:"filepath"`
	Tag      string `json:"tag"`
	Digest   string `json:"digest"`
}

// DeleteAgentRequest represents a request to delete an agent
type DeleteAgentRequest struct {
	FilePath string `json:"file_path"`
}

// DeleteAgentResponse represents the response from deleting an agent
type DeleteAgentResponse struct {
	FilePath string `json:"filePath"`
}

// SessionsResponse represents a session in the sessions list
type SessionsResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
	NumMessages  int    `json:"num_messages"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	WorkingDir   string `json:"working_dir,omitempty"`
}

// SessionResponse represents a detailed session
type SessionResponse struct {
	ID            string                     `json:"id"`
	Title         string                     `json:"title"`
	Messages      []session.Message          `json:"messages,omitempty"`
	CreatedAt     time.Time                  `json:"created_at"`
	ToolsApproved bool                       `json:"tools_approved"`
	InputTokens   int64                      `json:"input_tokens"`
	OutputTokens  int64                      `json:"output_tokens"`
	WorkingDir    string                     `json:"working_dir,omitempty"`
	Permissions   *session.PermissionsConfig `json:"permissions,omitempty"`
}

// UpdateSessionPermissionsRequest represents a request to update session permissions.
type UpdateSessionPermissionsRequest struct {
	Permissions *session.PermissionsConfig `json:"permissions"`
}

// ResumeSessionRequest represents a request to resume a session
type ResumeSessionRequest struct {
	Confirmation string `json:"confirmation"`
	Reason       string `json:"reason,omitempty"`    // e.g reason for tool call rejection
	ToolName     string `json:"tool_name,omitempty"` // tool name for approve-tool confirmation
}

// DesktopTokenResponse represents the response from getting a desktop token
type DesktopTokenResponse struct {
	Token string `json:"token"`
}

// ResumeElicitationRequest represents a request to resume with an elicitation response
type ResumeElicitationRequest struct {
	Action  string         `json:"action"`  // "accept", "decline", or "cancel"
	Content map[string]any `json:"content"` // The submitted form data (only present when action is "accept")
}

// SteerSessionRequest represents a request to inject user messages into a
// running agent session. The messages are picked up by the agent loop between
// tool execution and the next LLM call.
type SteerSessionRequest struct {
	Messages []Message `json:"messages"`
}

// LiveSessionNode represents a node in the currently-live agent/subagent tree.
type LiveSessionNode struct {
	SessionID       string    `json:"session_id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	RootSessionID   string    `json:"root_session_id"`
	AgentName       string    `json:"agent_name"`
	Title           string    `json:"title,omitempty"`
	Kind            string    `json:"kind"`
	Depth           int       `json:"depth"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at,omitzero"`
	LastUpdateAt    time.Time `json:"last_update_at,omitzero"`
	LastPreview     string    `json:"last_preview,omitempty"`
	Error           string    `json:"error,omitempty"`
}

// LiveSessionTreeResponse describes the current live tree for a root session.
type LiveSessionTreeResponse struct {
	RootSessionID string            `json:"root_session_id"`
	Nodes         []LiveSessionNode `json:"nodes"`
}

// LiveSessionResponse describes a single live session node (root or descendant).
type LiveSessionResponse struct {
	Node LiveSessionNode `json:"node"`
}

// LiveSessionSnapshotResponse describes the full current state of a live
// session: the live-tree node metadata (kind, agent, status) plus the current
// transcript. Used by remote clients that open the subagent in a normal chat
// tab and therefore need a populated [*session.Session] pointer locally, not
// just a live event subscription.
type LiveSessionSnapshotResponse struct {
	Node    LiveSessionNode `json:"node"`
	Session SessionResponse `json:"session"`
}

// LiveSessionControlResponse is returned by live-session control endpoints.
type LiveSessionControlResponse struct {
	Status string `json:"status"`
}

// UpdateSessionTitleRequest represents a request to update a session's title
type UpdateSessionTitleRequest struct {
	Title string `json:"title"`
}

// UpdateSessionTitleResponse represents the response from updating a session's title
type UpdateSessionTitleResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
