# Shared TUI applicability inventory

This tracked inventory records the product boundary used by the agentic-tui / Sandboxes port.

| Reference capability | docker-agent disposition | Owner |
|---|---|---|
| leased elapsed-time scheduler, delayed shrink | applicable; animation coordinator and root continuation | animation/runtime + integration |
| shell input sizing, tab/context/editor focus and mouse regions | applicable; retained in docker-agent root because tabs are runtime sessions | context/editor, tabbar + integration |
| layered card/dialog presentation, drag, resize, outside click | applicable; dialog manager layers and explicit non-closable policy | dialog parcels |
| ANSI edge/lifecycle fades | applicable; shared `styles` fade pipeline | scroll/dialog parcels |
| agentic project/artifact board, project selector | excluded: docker-agent has agents/sessions, no project/artifact product model | N/A |
| Sandboxes approval model and frame-count clock | excluded: runtime elicitation/tool decisions and elapsed-time leases are authoritative | N/A |

The root remains bespoke only where it adapts docker-agent session/agent runtime events; shared visual behavior is component-owned and composed there.
