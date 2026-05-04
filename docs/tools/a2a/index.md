---
title: "A2A Tool"
description: "Connect to remote agents via the Agent-to-Agent protocol."
permalink: /tools/a2a/
---

# A2A Tool

_Connect to remote agents via the Agent-to-Agent protocol._

## Overview

The A2A tool connects to remote agents via the A2A (Agent-to-Agent) protocol. It exposes remote A2A peers as tools so a local agent can delegate work across process or network boundaries.

## Configuration

```yaml
toolsets:
  - type: a2a
    name: research_agent
    url: "http://localhost:8080/a2a"
```

## Properties

| Property | Type   | Required | Description                    |
| -------- | ------ | -------- | ------------------------------ |
| `name`   | string | ✓        | Tool name for the remote agent |
| `url`    | string | ✓        | A2A server endpoint URL        |

<div class="callout callout-tip" markdown="1">
<div class="callout-title">💡 See also
</div>
  <p>For full details on the A2A protocol and serving agents as A2A endpoints, see <a href="{{ '/features/a2a/' | relative_url }}">A2A Protocol</a>.</p>
</div>
