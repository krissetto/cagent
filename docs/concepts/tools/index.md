---
title: "Tools"
description: "Tools give agents the ability to interact with the world — read files, run commands, search the web, query databases, and more."
permalink: /concepts/tools/
---

# Tools

_Tools give agents the ability to interact with the world — read files, run commands, search the web, query databases, and more._

## How Tools Work

When an agent needs to perform an action, it makes a **tool call**. The docker-agent runtime executes the tool and returns the result to the agent, which can then use it to continue its work.

1. Agent receives a user message
2. Agent decides it needs to use a tool (e.g., read a file)
3. docker-agent executes the tool and returns the result
4. Agent incorporates the result and responds

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ Tool Confirmation
</div>
  <p>By default, docker-agent asks for user confirmation before executing tools that have side effects (shell commands, file writes). Use <code>--yolo</code> to auto-approve all tool calls.</p>
</div>

## Built-in Tools

docker-agent ships with several built-in tools that require no external dependencies. Each is enabled by adding its `type` to the agent's `toolsets` list:

| Tool | Description |
| --- | --- |
| [Filesystem]({{ '/tools/filesystem/' | relative_url }}) | Read, write, list, search, and navigate files and directories |
| [Shell]({{ '/tools/shell/' | relative_url }}) | Execute arbitrary shell commands in the user's environment |
| [Think]({{ '/tools/think/' | relative_url }}) | Step-by-step reasoning scratchpad for planning and decision-making |
| [Todo]({{ '/tools/todo/' | relative_url }}) | Task list management for complex multi-step workflows |
| [Memory]({{ '/tools/memory/' | relative_url }}) | Persistent key-value storage backed by SQLite |
| [Fetch]({{ '/tools/fetch/' | relative_url }}) | Make HTTP requests to external APIs and web services |
| [Script]({{ '/tools/script/' | relative_url }}) | Define custom shell scripts as named tools |
| [LSP]({{ '/tools/lsp/' | relative_url }}) | Connect to Language Server Protocol servers for code intelligence |
| [API]({{ '/tools/api/' | relative_url }}) | Create custom tools that call HTTP APIs without writing code |
| [User Prompt]({{ '/tools/user-prompt/' | relative_url }}) | Ask users questions and collect interactive input |
| [Subagents]({{ '/tools/subagents/' | relative_url }}) | Runtime-managed, persistent, bidirectional multi-agent coordination |
| [A2A]({{ '/tools/a2a/' | relative_url }}) | Connect to remote agents via the Agent-to-Agent protocol |

<div class="callout callout-info" markdown="1">
<div class="callout-title">ℹ️ Multi-agent note
</div>
  <p>docker-agent's documented multi-agent model is <a href="{{ '/tools/subagents/' | relative_url }}">runtime-managed subagents</a>. Older compatibility surfaces remain in code for backward compatibility but are no longer documented as first-class tools here.</p>
</div>

## MCP Tools

docker-agent supports the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) for extending agents with external tools. There are three ways to connect MCP tools:

- **Docker MCP** (recommended) — Run MCP servers in Docker containers via the [MCP Gateway](https://github.com/docker/mcp-gateway). Browse the [Docker MCP Catalog](https://hub.docker.com/search?q=&type=mcp).
- **Local MCP (stdio)** — Run MCP servers as local processes communicating over stdin/stdout.
- **Remote MCP (SSE / HTTP)** — Connect to MCP servers running on a network. See [Remote MCP Servers]({{ '/features/remote-mcp/' | relative_url }}).

```yaml
toolsets:
  - type: mcp
    ref: docker:duckduckgo
```

See [Tool Config]({{ '/configuration/tools/#mcp-tools' | relative_url }}) for full MCP configuration reference.

<div class="callout callout-tip" markdown="1">
<div class="callout-title">💡 See also
</div>
  <p>For full configuration reference, see <a href="{{ '/configuration/tools/' | relative_url }}">Tool Config</a>. For the recommended multi-agent model, see <a href="{{ '/tools/subagents/' | relative_url }}">Subagents (Runtime-Managed)</a>. For RAG (document retrieval), see <a href="{{ '/features/rag/' | relative_url }}">RAG</a>.</p>
</div>
