# workflow

Durable Starlark workflows (design §6), executed with script-services
(shared dialect, step/time limits — `Options.Limits`, defaulting to `script.DefaultLimits`,
never unbounded). Importing the package registers it as the host's
workflow runner, so `host.RunWorkflow` and the loop's opt-in
`workflow` tool reach it.
`Register()` reinstalls that hook for hosts that replaced it.
`RunEx(ctx, host, name, input, opts)` is `Run` with `Options` and also
returns the workflow run ID to pass to `Resume` later.

A catalogued workflow is an `agentsdk.workflow@1` contribution of a
definition bundle (see [agentdef](agentdef.md)): its bundle.yaml declares
it as `application` or lists it as a contribution, and its id is the name `Run` takes. It is checked against the
workflow built-ins when the bundle loads, and `load()` reaches its own
bundle and the dependencies that bundle declares (starcompile). Ad-hoc
source passed to `Run` has no bundle, so it cannot `load()`. Resume
refuses a catalogued workflow whose source changed since the run
started. A top-level bundle `description` becomes the workflow's trace
description and is pinned on first run; it takes precedence over a script's
optional `workflow(description = ...)` value.

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

## Installed chat workflows

Declare `chat=True` in `WORKFLOW = workflow(...)`. The chat script receives
`input.message` and calls its own named roles with `agent(name, task=...,
phase=..., output=...)`, or side by side with `parallel([spec(name, task=...),
...])`, composing their text with `render` and `json`. `output` is a schema
the role must complete against, read back as `result.output`. Return the
final role result.

A chat workflow's roles are the parts its agents play. Declare them with
`roles = [role(name, description=..., agent=..., seats=...)]`: `agent` plays
the role unless a turn casts another (an agent of any bundle, usually the
workflow's own), and `seats` is how many agents play it (1 by default, or
`"all"` for every installed agent but the workflow's own, and but the
agents any chat workflow brings to play a role it declares, such as a
coordinator or a merger: `agentdef.EveryAgent`). A workflow that
declares no roles has one per agent of its own bundle, played by that
agent. Each turn may cast other installed agents in any role with
`host.WithCast({role: [agents]})`, as many as it likes in each; a role it
leaves out is played as declared. The workflow reads who plays each role
as `input.cast`, by role a list of `{name, description}`, and calls a role
by name (`agent("coordinator", ...)` runs the first agent in its seats) or
an agent cast in it by its own name. Casting a role it doesn't have, or an
agent that isn't installed, fails the turn; so does calling a role nobody
plays, or an agent nobody cast that isn't the workflow's own. The
workflow's own agents run their authored loops; an agent of another bundle
runs as that bundle wrote it.

`host.WithSettings({field: value})` gives the turn the workflow's other
input fields (anything but `message` and `cast`), checked against its
`input` schema: a host keeps them per chat as the workflow's settings.

A chat workflow whose `run` only hands the message to one agent
(`return agent("name", task = input.message)`, no settings, at most that
agent's one seat) is a one-to-one chat with that agent:
`Workflow.Delegate` names the agent, so a host can list the one in place
of the other.

Every agent the turn runs may call the agents cast in its roles as
subagents (`agent`, `agent_spawn`, told whom it may call), and so may the
agents they start. Each agent keeps its own thread in the chat
(`host.MemberThread`) from turn to turn, whichever role it plays, and an
agent cast started as a subagent carries on in that thread, unless it is
running at that moment, when it starts fresh. Role definitions resolve against one installed
catalogue snapshot, including loop and skill inheritance across declared bundle
dependencies. Interactive sends share a budget and retain a conversation thread
per role. Standalone replay verifies the catalogue digest before reusing the
journal. Installing a different role revision invalidates that replay.
