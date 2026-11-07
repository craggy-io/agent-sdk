# examples/host

A runnable host wiring the whole stack: a capability-services registry,
an agent host whose tools are a registry view, the
[agentcap](agentcap.md) bridge, optional hot-reloaded capability
scripts, durable trace and memory, and either HTTP or MCP in front. It
is its own module, so its dependencies (MCP SDK, SQLite) stay out of
the core.

```sh
cd examples/host
export OPENROUTER_API_KEY=...       # optional: without it the server runs, sampling fails clearly
go run . -addr :8080                # HTTP API, OpenAPI at /v1/openapi.json
go run . -mcp                       # MCP tools on stdio
go run . -defs ./mydefs -scripts ./caps -trace trace.db -memory ./mem -config session.example.yaml
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:8080` | HTTP listen address |
| `-mcp` | off | serve MCP over stdio instead of HTTP |
| `-defs` | none | user definition layer: a tree of bundles replacing built-ins by name |
| `-approve-promote` | off | approve `agents.promote` / `workflows.promote` writes under `-defs` (logged) |
| `-scripts` | none | capability script bundle, loaded and hot-reloaded |
| `-trace` | in-memory | SQLite trace file; needed for resume across restarts |
| `-memory` | in-memory | durable memory directory |
| `-config` | built-in defaults | session config YAML, see [`session.example.yaml`](../examples/host/session.example.yaml) and [config](config.md) |

## Views

Agents and MCP clients get the agent view: everything except
capabilities tagged `admin` or `host`, so they cannot promote
definitions or answer their own `ask` gates. HTTP gets the full view
and takes the caller from an `X-User` header. That is for the example
only; a real deployment puts authentication in front of it.

Promotions install each definition as its own bundle under `-defs`
(`definitions/agentsdk.user.definitions.<name>/`,
`workflows/agentsdk.user.workflows.<name>/`) and only when
`-approve-promote` is set.
