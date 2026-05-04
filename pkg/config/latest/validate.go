package latest

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"
)

func (t *Config) UnmarshalYAML(unmarshal func(any) error) error {
	type alias Config
	var tmp alias
	if err := unmarshal(&tmp); err != nil {
		return err
	}
	*t = Config(tmp)
	return t.validate()
}

// UnmarshalYAML accepts both the canonical `subagents` key and the legacy
// `sub_agents` spelling. The canonical key wins whenever it is present,
// including when it is explicitly set to an empty list.
func (a *AgentConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type alias AgentConfig
	var raw map[string]any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if _, hasCanonical := raw["subagents"]; !hasCanonical {
		if legacy, hasLegacy := raw["sub_agents"]; hasLegacy {
			raw["subagents"] = legacy
		}
	}
	delete(raw, "sub_agents")

	data, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	var tmp alias
	if err := yaml.UnmarshalWithOptions(data, &tmp, yaml.DisallowUnknownField()); err != nil {
		return err
	}
	*a = AgentConfig(tmp)
	return nil
}

// UnmarshalJSON mirrors UnmarshalYAML so that transport paths that go through
// JSON (e.g. `types.CloneThroughJSON` during version upgrades and the HTTP API)
// also accept the legacy `sub_agents` key.
func (a *AgentConfig) UnmarshalJSON(data []byte) error {
	type alias AgentConfig
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, hasCanonical := raw["subagents"]; !hasCanonical {
		if legacy, hasLegacy := raw["sub_agents"]; hasLegacy {
			raw["subagents"] = legacy
		}
	}
	delete(raw, "sub_agents")

	normalized, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var tmp alias
	if err := json.Unmarshal(normalized, &tmp); err != nil {
		return err
	}
	*a = AgentConfig(tmp)
	return nil
}

func (t *Config) validate() error {
	for i := range t.Agents {
		agent := &t.Agents[i]

		// Validate fallback config
		if err := agent.validateFallback(); err != nil {
			return err
		}

		for j := range agent.Toolsets {
			if err := agent.Toolsets[j].validate(); err != nil {
				return err
			}
		}

		// Cross-cutting check: runtime-managed subagents are the replacement
		// for the legacy `transfer_task` (historically auto-injected when
		// `subagents` / `sub_agents` is set), `background_agents`, and
		// `handoffs` flows. Mixing them on the same agent is not supported.
		if err := agent.validateSubagentsExclusivity(); err != nil {
			return err
		}

		if agent.Hooks != nil {
			if err := agent.Hooks.validate(); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateSubagentsExclusivity enforces that the runtime-managed subagents
// subsystem (opted into by the presence of `subagents:` / `sub_agents:`)
// is not combined with the legacy multi-agent surfaces on the same agent.
func (a *AgentConfig) validateSubagentsExclusivity() error {
	if len(a.SubAgents) == 0 {
		return nil
	}
	if len(a.Handoffs) > 0 {
		return fmt.Errorf("agent %q: cannot combine runtime-managed `subagents:` with `handoffs:`; handoffs is a legacy multi-agent flow that runtime-managed subagents replaces", a.Name)
	}
	for i := range a.Toolsets {
		switch a.Toolsets[i].Type {
		case "background_agents":
			return fmt.Errorf("agent %q: cannot combine runtime-managed `subagents:` with `- type: background_agents`; background_agents is a legacy multi-agent flow that runtime-managed subagents replaces", a.Name)
		case "subagents":
			return fmt.Errorf("agent %q: `- type: subagents` is no longer valid; top-level `subagents:` now both declares available subagents and enables the runtime-managed subagent tools", a.Name)
		}
	}
	return nil
}

// validateFallback validates the fallback configuration for an agent
func (a *AgentConfig) validateFallback() error {
	if a.Fallback == nil {
		return nil
	}

	// -1 is allowed as a special value meaning "explicitly no retries"
	if a.Fallback.Retries < -1 {
		return errors.New("fallback.retries must be >= -1 (use -1 for no retries, 0 for default)")
	}
	if a.Fallback.Cooldown.Duration < 0 {
		return errors.New("fallback.cooldown must be non-negative")
	}

	return nil
}

func (t *Toolset) validate() error {
	// Attributes used on the wrong toolset type.
	if len(t.Shell) > 0 && t.Type != "script" {
		return errors.New("shell can only be used with type 'script'")
	}
	if t.Path != "" && t.Type != "memory" && t.Type != "tasks" {
		return errors.New("path can only be used with type 'memory' or 'tasks'")
	}
	if len(t.PostEdit) > 0 && t.Type != "filesystem" {
		return errors.New("post_edit can only be used with type 'filesystem'")
	}
	if t.IgnoreVCS != nil && t.Type != "filesystem" {
		return errors.New("ignore_vcs can only be used with type 'filesystem'")
	}
	if len(t.Env) > 0 && (t.Type != "shell" && t.Type != "script" && t.Type != "mcp" && t.Type != "lsp") {
		return errors.New("env can only be used with type 'shell', 'script', 'mcp' or 'lsp'")
	}
	if len(t.FileTypes) > 0 && t.Type != "lsp" {
		return errors.New("file_types can only be used with type 'lsp'")
	}
	if len(t.Models) > 0 && t.Type != "model_picker" {
		return errors.New("models can only be used with type 'model_picker'")
	}
	if t.Shared && t.Type != "todo" {
		return errors.New("shared can only be used with type 'todo'")
	}
	if t.Version != "" && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("version can only be used with type 'mcp' or 'lsp'")
	}
	if t.Command != "" && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("command can only be used with type 'mcp' or 'lsp'")
	}
	if len(t.Args) > 0 && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("args can only be used with type 'mcp' or 'lsp'")
	}
	if t.Ref != "" && t.Type != "mcp" && t.Type != "rag" {
		return errors.New("ref can only be used with type 'mcp' or 'rag'")
	}
	if (t.Remote.URL != "" || t.Remote.TransportType != "" || t.Remote.OAuth != nil) && t.Type != "mcp" {
		return errors.New("remote can only be used with type 'mcp'")
	}
	if (len(t.Remote.Headers) > 0) && (t.Type != "mcp" && t.Type != "a2a") {
		return errors.New("remote headers can only be used with type 'mcp' or 'a2a'")
	}
	if len(t.Headers) > 0 && t.Type != "openapi" && t.Type != "a2a" {
		return errors.New("headers can only be used with type 'openapi' or 'a2a'")
	}
	if t.Config != nil && t.Type != "mcp" {
		return errors.New("config can only be used with type 'mcp'")
	}
	if t.URL != "" && t.Type != "a2a" && t.Type != "openapi" {
		return errors.New("url can only be used with type 'a2a' or 'openapi'")
	}
	if t.Name != "" && (t.Type != "mcp" && t.Type != "a2a" && t.Type != "rag") {
		return errors.New("name can only be used with type 'mcp', 'a2a', or 'rag'")
	}
	if t.RAGConfig != nil && t.Type != "rag" {
		return errors.New("rag_config can only be used with type 'rag'")
	}

	switch t.Type {
	case "shell":
		// no additional validation needed
	case "memory":
		// path is optional; defaults to ~/.cagent/memory/<agent-name>/memory.db
	case "tasks":
		// path defaults to ./tasks.json if not set
	case "mcp":
		count := 0
		if t.Command != "" {
			count++
		}
		if t.Remote.URL != "" {
			count++
		}
		if t.Ref != "" {
			count++
		}
		if count == 0 {
			return errors.New("either command, remote or ref must be set")
		}
		if count > 1 {
			return errors.New("either command, remote or ref must be set, but only one of those")
		}
		if t.Remote.OAuth != nil {
			if t.Remote.URL == "" {
				return errors.New("oauth requires remote url to be set")
			}
			if t.Remote.OAuth.ClientID == "" {
				return errors.New("oauth requires clientId to be set")
			}
			if t.Remote.OAuth.CallbackPort != 0 && (t.Remote.OAuth.CallbackPort < 1 || t.Remote.OAuth.CallbackPort > 65535) {
				return errors.New("oauth callbackPort must be between 1 and 65535")
			}
		}
	case "a2a":
		if t.URL == "" {
			return errors.New("a2a toolset requires a url to be set")
		}
	case "lsp":
		if t.Command == "" {
			return errors.New("lsp toolset requires a command to be set")
		}
	case "openapi":
		if t.URL == "" {
			return errors.New("openapi toolset requires a url to be set")
		}
	case "model_picker":
		if len(t.Models) == 0 {
			return errors.New("model_picker toolset requires at least one model in the 'models' list")
		}
	case "rag":
		// rag toolset requires either a ref or inline rag_config
		if t.Ref == "" && t.RAGConfig == nil {
			return errors.New("rag toolset requires either ref or rag_config")
		}
	case "background_agents":
		// no additional validation needed
	}

	return nil
}
