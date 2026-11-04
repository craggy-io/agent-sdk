# workflow

Durable Starlark workflows (design §6), executed with script-services
(shared dialect, `load()` against the cataloguing root, step/time
limits — `Options.Limits`, defaulting to `script.DefaultLimits`,
never unbounded). Importing the package registers it as the host's
workflow runner, so `host.RunWorkflow` and the loop's opt-in
`workflow` tool reach it.

A workflow file declares `WORKFLOW = workflow(description, input,
agents, profiles, limits)` (`input` is a field dict of `schema.*`
values; the input schema is closed) and `def run(input)`. Input and
results are attribute-accessible objects (`input.text`,
`result.usage`).

Built-ins: `agent`, `spec`, `start`, `wait`, `parallel` (branches
run on Go goroutines; Starlark waits), `define`, `workflow_run`,
`ask` (human gate: suspends with `StatusWaiting`; `Answer` then
`Resume`), `call` and `tools.*` (journaled tool calls through the
workflow's tool view), `spawn`/`send`/`interrupt`/`resume`/`close`/
`status`/`list_agents`, plus `phase`, `log`, `budget`, `config`,
`render`, `json`, `schema`.

Resume replays the journal (see design §6): completed calls return
recorded values, ask answers persist, and operations with unknown
outcomes block until `ResolveRecovery`. `ListWaiting` rebuilds
pending gates after a restart. Errors a script must recover from
come back as values (tool results carry `is_error`); Go errors abort
the run, because Starlark has no handlers.
