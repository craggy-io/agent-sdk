# Agent SDK: coding-agent integration guide

Use this guide when editing this repository or integrating its Go module,
`github.com/craggy-io/agent-sdk`, into a consumer. Start with the
[README quick start](README.md#quick-start) and the executable example in
`agentdef/example_test.go`, which `go test` compiles and checks.

## Choose the owning package

| Need | Package and contract |
| --- | --- |
| Talk to a model; define requests, tools, usage | [model](docs/model.md) |
| Store or transform conversation history | [transcript](docs/transcript.md) |
| Run the ReAct loop directly | [loop](docs/loop.md) |
| Record or replay runs | [trace](docs/trace.md) |
| Profiles, limits, budgets as Go values | [config](docs/config.md) |
| Define agents in Starlark | [agentdef](docs/agentdef.md) |
| Own threads, runs, peers, events | [host](docs/host.md) |
| Durable multi-agent workflows in Starlark | [workflow](docs/workflow.md) |
| Durable memory for agents and scripts | [memory](docs/memory.md) |
| Expose agents/workflows/LLM/memory as capabilities | [agentcap](docs/agentcap.md) |
| Fake a provider in tests | [agenttest](docs/agenttest.md) |
| Inspect a trace database from a shell | [craggy-agent](docs/cli.md) |

## The import rule

Dependencies point one way. `model`, `transcript`, `config` and `trace`
import nothing else from this module (beyond `model`). `loop` builds on
`model`, `transcript` and `trace`. `host` builds on all of them plus
`agentdef`; `workflow` and `agentcap` build on `host`. Domain packages never
import capability-services; only `host` (tool view over a registry view) and
`agentcap` (the bridge) do. This module never imports a consumer; its
craggy-io dependencies are foundation-kit, script-services and
capability-services (`arch_test.go` enforces the list).

- foundation-kit's `bundle` loads definition content; agentdef declares
  its kinds and `Open` holds the layers in a `bundle.Live`. Never add a
  second loader or read definition files directly.
- script-services runs and checks scripts; it knows nothing of agents.
  Agent and workflow files compile through its `starcompile` compiler,
  never through a `modules.NewLoader` built here.
  Host effects reach scripts only as `script.Program.Predeclared` built-ins
  with a matching static description (`check.Callable` / starlarkcap views).
- capability-services owns registration, views, jobs and projections.
  Register capabilities there; never invent a second registry here.
- Secrets never enter this module: providers take keys at construction,
  config is Go values, and nothing in the library packages reads environment
  variables; only the gated live test, `cmd/craggy-agent` and `examples/host`
  read `OPENROUTER_API_KEY`.

## Construct the pieces

1. Provider: `openrouter.New(key)` or any `model.Provider`; tests use
   `agenttest.NewScripted(...)`.
2. Trace sink: `trace/memory.New()` for tests, the SQLite sink in
   `adapters/tracesqlite` for durable runs. One writer owns a sink; close it
   after the host.
3. Definitions: `agentdef.Open(ctx, agentdef.Options{Shipped: defs.Sources(),
   UserDir: ..., Project: ...})`; leave out the other layers for the
   embedded set alone. Definitions name profiles, never models. See "Definition bundles".
4. Host: `host.Open(ctx, host.Options{...})`; close in reverse order
   (host, then sink, then module sources).
5. Capabilities (optional): `agentcap.Register(r, h, agentcap.Options{...})`
   adds `agents.*`, `workflows.*`, `sessions.*`, `recovery.*`, `llm.*`,
   `memory.*` and `trace.*` to a
   capability-services registry. Runtime `define`/`promote` writes nothing
   unless `Options.Approve` approves it.

## Definition bundles

- **Every definition folder needs a `bundle.yaml`** declaring one
  `application` or listing each agent
  (`agentsdk.agent@1`), skill (`agentsdk.skill@1`, a `SKILL.md`) and
  workflow (`agentsdk.workflow@1`) as a contribution whose `id` is its
  name. Never generate a manifest from a folder layout or file names, in
  code or tests: fixtures write their `bundle.yaml` bytes.
- One agent per file: `agent(...)` exactly once, `name` equal to the
  contribution id. Share helpers through `load()` of undeclared support
  files in the same bundle, or `@<dependency-id>/path` from a declared
  dependency.
- Layers contain multiple bundles with independent IDs. `Shipped` is a source
  list; `UserDir` and `Project` are recursively discovered bundle trees. Higher
  layers override lower contributions by kind and name using explicit bundle
  overrides. Same-layer conflicts fail. Shared support belongs in declared
  dependency bundles; cross-layer libraries go in `Options.Sources`.
- Runtime changes use `bundle.Live`: `Reload`, `AddDefinition` (Go definitions)
  and `InstallAgent`/`InstallWorkflow` (one user bundle per promoted definition,
  updated through `Live.Edit`). Resolved runs pin prompt, schema and skill bytes.

## Semantics that must not drift

- The loop settles every started tool call exactly once, appends results
  contiguously in call order, and never dispatches on truncated, errored or
  unfinished samples. An empty completion is never success.
- Tool authority only narrows: host view, intersected with the definition
  filter, intersected with the call-site filter. Unknown effects fail closed.
- Workflow built-ins journal intent before dispatch and results before
  return. Replay returns recorded values, failures included. A mutation
  whose outcome is uncertain blocks resume until explicitly resolved.
- Starlark has no error handling: built-ins that scripts must recover from
  return error values, never Go errors. Scripts cannot recurse and hold no
  state across runs; state lives in transcripts, memory and the journal.
- Budgets charge before work and are shared across a whole run tree.

## Repository rules

- Search with `rg`. Extract a helper on the second occurrence of a pattern.
  When a data model changes, trace every consumer (formatters, templates,
  normalisers, docs) before considering the change done.
- Fully remove obsolete code and all call sites; no deprecation shims.
- Verify fixes by running tests. The gate is the block in the README
  Development section, including the nested `adapters/tracesqlite` module.
- Example tests (`example_test.go`) carry `// Output:` checks; add one when a
  doc snippet needs to stay true.
