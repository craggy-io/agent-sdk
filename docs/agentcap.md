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

Runtime registration: `agents.define` takes Starlark `source`
(exactly one `agent(...)` declaration, extending a catalogued
definition) or a JSON `definition`; `workflows.define` takes a name
and source. Both are session-scoped. `promote` installs the agent or
workflow in the user layer of the host's loader
(`agentdef.Options.UserDir`): the file, `agents/<name>.star` or
`workflows/<name>.star`, and its entry in the user bundle's
`bundle.yaml`, written under `<UserDir>/agentsdk.user` with
`bundle.Live.Edit` (`Loader.InstallAgent`/`InstallWorkflow`). It runs only when
`Options.Approve` accepts the exact bytes (validated first: agents
must parse and resolve), and the install is rolled back if the
catalogue then fails to load. Without a user layer, promote is
unavailable. JSON-defined agents are rendered to Starlark for
promotion.
