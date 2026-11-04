# host

The runtime root (design §2, §11). `Open(ctx, Options{Config, View,
Provider, Defs, Trace, Caller, ID})` acquires the trace writer lease
and rebuilds threads/handles from the trace; `Close` in reverse
order (host, then sink).

Execution: `RunAgent` (name or dynamic definition, with
`WithThread`/`WithProfile`/`WithVars`/`WithTools`/`WithOutput`
options) and the general `ExecAgent(ExecSpec)`. One-shots and spawned
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
