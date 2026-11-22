# host

The runtime root (design §2, §11). `Open(ctx, Options{Config, View,
Provider, Defs, Trace, Caller, ID})` acquires the trace writer lease
and rebuilds threads/handles from the trace; `Close` in reverse
order (host, then sink).

Execution: `RunAgent` (name or dynamic definition, with
`WithThread`/`WithProfile`/`WithVars`/`WithTools`/`WithOutput`/`WithFills`/`WithSkills`
options; `WithFills` fills slots and `WithSkills` gives skills no
definition names, each for this agent only, not the agents it delegates
to, see agentdef's "Slots" and "Skills given to a run") and the general `ExecAgent(ExecSpec)`. One-shots and spawned
handles share the run tree, budget and concurrency semaphore.

Control: `Spawn`, `SendInput` (durable `Receipt`; modes `message`,
`steer`, `follow_up`), `Interrupt`, `Resume`, `CloseHandle`,
`Status`, `ListAgents`, `Wait`, `Peers`, `Inputs`. The same surface
is what loop delegate tools and workflow built-ins call, with
ownership checks in one place (`RequireAgentAccess`).

Tools: `ToolView` is the host's tool seam. `NewRegistryView(view)`
projects a capability-services registry view (tools re-listed per
call, effects mapped, jobs waitable, agent caller stamped);
`NewMapView` serves plain Go functions. `Narrow` intersects filters;
authority only narrows.

Events: `Events(ctx, EventOptions)` streams durable trace events
from a cursor plus live deltas (bounded per subscriber; slow
subscribers disconnect with a gap marker). `SetConfig` hot-applies a
new config to future runs.


## Chat turns

Import `github.com/craggy-io/agent-sdk/workflow` for its registration when
integrating the host directly. `RunAgent` and `ExecAgent` execute the embedded
`agent-turn` workflow. `RunChat` selects an installed workflow declared with
`workflow(chat=True, ...)`. `WithCast(cast)` puts installed agents in the
workflow's roles for the turn and `WithSettings(values)` gives it its other
input fields (see [workflow](workflow.md)). The agents cast are the turn's
`ExecSpec.Group`, which lets a run, and every agent it starts, delegate to
them beside its definition's `delegates`; `ExecSpec.GroupThread` is the
chat under which each keeps its thread, `MemberThread(chat, agent)`. Each role gets its own persistent thread under the
caller's chat thread key. An interactive turn can continue after host restart
by sending another message to the same chat. Standalone workflow replay of an
interactive turn is refused because its caller-supplied callbacks belong to the
original chat.
