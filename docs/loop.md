# loop

The Starlark loop runtime (design §5). `Run(Deps, Input)` where `Deps` are a
`model.Provider`, a `ToolRunner` (the loop's narrow tool seam:
`Tools`/`Groups`/`Run`/`WaitJob`), an optional trace recorder,
budget and delegate. `Policy` carries max steps, completion mode
(text or structured with a `complete` tool and output schema),
compaction and clearing thresholds, progressive tool disclosure,
budget-notice fractions and hooks.

Guarantees worth relying on: every started call settles exactly once
on every exit path; results append contiguously in call order;
read-only calls may overlap, others run serially; truncated or
errored samples never dispatch; an empty completion is never
success. Built-in tools: `complete` (structured mode), `skill`,
`result_read`, `tools_load`, plus whatever the `Delegate` advertises
(the host's agent/workflow controls).

Each request is sent the run's own `Input.System`, in place of the
system item the thread was started with; the stored transcript keeps
the first. A conversation is therefore always held to its agent as it
resolves now: an edited definition, or a slot filled by another filler
(`agentdef` "Slots"), reaches the next turn. `Policy.PinnedTools`
(names, each may end in `*`) are never deferred by progressive
loading, whatever their group.

## Executable behavior

A bundle loop exports `run(step, max_steps)`. Calling `step()` samples the
current thread. It returns a status and a value. On `actions`, the value is a
zero-argument function that executes exactly the calls from that response,
once. On `continue`, the loop may sample again. Other statuses end the turn.
An intermediate sample uses `step(instruction, intermediate=True)`. It can
return `answer` with text for the script to use in its next instruction.

Authored loops have access to their pinned bundle skills, local result reads
and the host's configured completion mechanism. The host settles outstanding
calls if the script stops, fails or is cancelled. The embedded default behavior
also runs as Starlark and uses the host's existing tool view.
