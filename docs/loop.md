# loop

The one ReAct loop (design §5). `Run(Deps, Input)` where `Deps` are a
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
