# agentcap

`Register(r, host, Options)` adds the host's surface to a
capability-services registry: `agents.*` (list/get/run/spawn/send/
interrupt/resume/close/status/handles/wait/define/promote),
`workflows.*` (list/get/define/run/pending/answer/promote),
`sessions.*`, `recovery.*`, `trace.*`, `llm.*` (provider-neutral
one-shot sampling by profile, model info, profile list) and
`memory.*`, plus their groups. `agents.run` and `workflows.run` are
long jobs; everything else is synchronous.

Authority: choose per-projection views in the host application.
Promote carries the `admin` tag and destructive effect;
`workflows.pending`/`answer` carry `host`. Keep both out of agent
views — agents propose, hosts approve.

Runtime registration: `agents.define` takes Starlark `source` (one
or more `agent(...)` declarations, each extending a loaded
definition) or a JSON `definition`; `workflows.define` takes a name
and source. Both are session-scoped. `promote` writes
`<Root>/agents/<name>.star` or `<Root>/workflows/<name>.star` only
when `Options.Approve` accepts the exact bytes (validated first:
agents must parse and resolve), then reloads the catalogue.
JSON-defined agents are rendered to Starlark for promotion.
