# flow

A pipeline is a bundle of files with one `pipeline.star`: a `pipeline()`
header (title, inputs, the capabilities it requires) and a `STEPS` list,
run by `run(ctx)` through the Starlark library `@craggy/flow.star`. Each
step is a `flow.<kind>(...)` call over one capability. Craggy Studio edits
and runs pipelines in the cloud; the harness runs the same folders in its
core and edits them with `core.pipelines.*`.

`flow` reads, edits, checks and describes pipelines. It runs nothing: a
host runs `pipeline.star` its own way and gives it the built-ins the
library needs.

```go
b, _ := flow.ReadDir(dir)                        // or a Bundle from a store
h, _ := flow.ReadHeader([]byte(b[flow.FileName]))
fl, _ := flow.ReadFlow([]byte(b[flow.FileName])) // fl.Steps, fl.Functions
b, summary, _ := flow.Apply(b, ops)               // add, edit, move, remove, file, input, title
problems := flow.Validate(b, flow.Env{Host: "Craggy Studio", Served: served, Forbid: []string{"studio."}})
```

## The format

- `STEPS` holds only constant `flow.<kind>(...)` calls with keyword
  arguments, so it reads back as steps and writes out canonically:
  `Flow.Source` replaces only the list and keeps the rest of the file byte
  for byte. A function a step can't express is a Code step (`fn = name`).
  A file with no `STEPS` is a hand-written `run()`.
- `Kinds` are the step kinds with their parameters, in `flow.star`'s
  order: read, for_each, agent, workflow, approve, ask, embed, keep_if,
  group, sort, save, call, code. `Step.Calls` is the capability a step calls, `Step.Reads`
  the variables it reads, and `Step.Line` where it is written.
- `call` calls any capability the host serves: `args` are fixed
  arguments and `bind` maps argument names to variables of earlier steps
  (`{"id": "post.url"}`). A host's step menu offers every capability this
  way, beside the named kinds.
- Inputs are `flow.input(dataset, key = ...)`, a craggy-data dataset's
  rows, or `flow.param(type, label = ..., default = ...)`, one value the
  run is given (`ValueTypes`: text, number, bool, lab, artifact,
  artifacts, json). A read step returns either.
- `agent` runs an agent made for the step, or with `agent = "<id>"` an
  installed agent with its own tools (`studio.agent` gets `agent` in its
  input). `workflow` runs an installed workflow on an earlier step's
  output. `approve` calls `core.pipelines.approve@1` with its question
  filled in and what `show` names; No ends the run there (an event with
  `stopped`). It can't sit inside a For each.
- Edits keep `requires` whole: `Apply` adds each capability the steps
  call (`Needs`), never removes one.
- Steps travel as JSON: `{"kind": ..., <args in order>}`, a Code step's
  function as `{"$ref": name}`; an argument equal to its default is left out.

## Hosts

A host gives a run:

- the built-ins `call(ref, **args)` (over the capabilities the pipeline
  requires), `struct`, `json` and a `pipeline` that does nothing;
- `Bundle.Modules()` to load (`Lib` as `@craggy/flow.star`, and the
  bundle's own `.star` files);
- `ctx = {"inputs": {...}, "files": Bundle.Flow(), "mode": ...}`, with the
  inputs read once before the run, each row marked by `Keyed`, and the
  value inputs from `Values` (what the run was given, else defaults).
  `TestInputs` reads a test run's datasets from `testdata/inputs.json`;
  `Input.Check` refuses a key or filter a query mustn't take.

It reads progress from the run's `print()` lines with `ReadEvent`: a step
starting (with the item and its input keys inside a For each) or done with
the count it made.

`CallKey` is a call's content address (capability and canonical input):
what a host keys a run's answers by, as Studio's step cache and the
harness's run journal do.

`Env` holds what only the host knows: its name for messages, what it
serves, its models and their output caps, its datasets, namespaces a
pipeline may never name (`Forbid`), whether it gives value inputs
(`Values`), and its installed agents and workflows (`Agent`, `Workflow`).
A host without them refuses steps that need them.
