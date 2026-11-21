# Plan: one flow library for Studio and the harness

Status: being built (2026-11-21). Section 6 records what was done.

## 1. Why

Craggy Studio's pipelines are a `pipeline.star` whose `STEPS` list is a flow
of steps (`flow.read`, `flow.for_each`, `flow.agent`, …) run by the Starlark
library `@craggy/flow.star`. Studio reads that list as steps, edits it and
writes it back (the Flow tab and its assistant). The harness runs the same
pipelines in its core (`core.pipelines.run`).

Today the code is in two places:

- `craggy-studio/src/pipeline`: the bundle, the `pipeline()` header, the
  step model, the source round trip, edit ops, kinds and validation. The
  harness has none of it, so nothing there can build or edit a flow.
- `craggy-harness/src/internal/pipelines`: a byte-for-byte copy of
  `flow.star` kept in step by a test against a sibling checkout, its own
  `Header`/`Input` types and header reader (it runs the module body to
  read `pipeline()`), its own `flow:` progress reader, test-input reader
  and row keying, all of which Studio also has.

## 2. Where it lives: `agent-sdk/flow`

- Not foundation-kit: it sits under script-services and has no Starlark.
- Not script-services: it keeps host semantics out, and step kinds name
  capabilities and say whether they spend.
- agent-sdk already holds workflows over capabilities and imports
  script-services. `flow` imports nothing else from agent-sdk and never
  capability-services (agent-sdk's import rule): it reads, edits, checks
  and describes flows, and hands hosts the library and the progress
  format. Running stays with each host, which already runs programs its
  own way (Studio: `starlarkcap.Exec` with metering; harness:
  `scripts.Runner` as the caller).

## 3. What the package holds

| Piece | From |
| --- | --- |
| `Bundle` (files by path, `Digest`, `Paths`, `Flow`, `Clone`), `CheckPath`, `FileName`, `Module`, limits | Studio `bundle.go` |
| `Header`, `Input`, `ReadHeader`, `SetHeader`, `RenderHeader`, `Template` | Studio `bundle.go`; replaces the harness's run-the-body reader |
| `Step`, `Arg`, `Ref`, `Flow`, `ReadFlow`, `Source`, `Walk`, `Find`, step JSON | Studio `flow.go` |
| `Kind`, `Param`, `Kinds`, `KindOf`, `Ops`, `Step.Output`, `Step.Calls` | Studio `kinds.go`, plus the new `call` kind |
| `Op`, `Apply`, `Needs` | Studio `edit.go` |
| `Env`, `Problem`, `Validate` | Studio `validate.go`, with the Studio-only rules made `Env` fields |
| `Lib` (`flow.star`, embedded) | Studio `res/lib/flow.star`; the harness copy and its sync test go |
| `Event`, `ReadEvent` (the `flow:` lines) | both hosts' progress readers |
| `TestInputs`, `Keyed`, `Input.Check` | both hosts' input readers |

Studio-only rules become host settings in `Env`:

- `Host` names the host in messages ("requires X, which Craggy Studio
  doesn't offer").
- `Forbid` lists capability namespaces a pipeline may never name (Studio:
  `studio.`), checked in every `.star` file outside comments.

## 4. Every capability: the `call` kind

The named kinds stay as friendly forms over the capabilities they wrap.
A new kind opens a flow to every capability the host serves:

```python
flow.call(id = "fetch", capability = "artifacts.get@1", args = {"kind": "note"}, bind = {"id": "post.id"}, out = "doc")
```

- `args` are constant arguments; `bind` maps an argument name to a
  variable (or a dotted path into one) read when the step runs.
- `Step.Calls()` is the capability a step calls: its kind's, or a call
  step's own. `Needs` adds it to `requires`, and `Validate` checks it is
  served, required and well formed, and that bound variables exist.
- Studio's catalogue labels a capability without a kind of its own with
  the `call` kind's label, as it did with "Call a capability" before.

## 5. Cut-over

1. **agent-sdk**: `flow` with Studio's pipeline tests moved over, the
   `call` kind tested end to end through a Starlark run of `Lib`, an
   import rule in `arch_test.go`, `docs/flow.md`, AGENTS.md and README rows.
2. **craggy-studio**: `src/pipeline` deleted. Callers import
   `agent-sdk/flow`; `res/lib/flow.star` deleted (the runtime loads
   `flow.Lib`); `runs` reads progress with `flow.ReadEvent` and test inputs
   with `flow.TestInputs`. Validation passes `Host: "Craggy Studio"`,
   `Forbid: ["studio."]`.
3. **craggy-harness**: `internal/pipelines` keeps only what is the
   harness's (reading a folder, data reads as the caller, packs, the run
   log); its `flow.star`, `Header`, `Input`, header reader, progress reader
   and `keyed` go. New core capabilities so agents and pages can build
   flows over the harness's own capabilities:
   - `core.pipelines.kinds`: the step kinds.
   - `core.pipelines.read`: a folder's header, steps, functions and
     problems, checked against what the caller may call.
   - `core.pipelines.edit`: ops applied to a folder's files, written back,
     with the one-line summary.
   - `core.scripts.describe` gives a pipeline's steps (`kind: pipeline`)
     so the script workbench shows them in a Steps view.
4. Checks in each repo, commit and push in dependency order: agent-sdk,
   then Studio and the harness (their `go.mod` move to the new agent-sdk).

## 6. Status

Being built.
