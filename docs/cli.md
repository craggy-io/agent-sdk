# craggy-agent CLI

`cmd/craggy-agent` (its own nested module) is the trace debugger and
live smoke runner:

```
craggy-agent [--root <dir>] [--db trace.db|--mem] [--config session.yaml] run (--agent <name>|--workflow <name>) <task...>
craggy-agent --db trace.db resume <workflow-run-id>
craggy-agent --db trace.db runs|tree <run>|step <step> --context|--diff|tools <node>|usage|replay|grep|inputs|recovery|tail|agents
```

Live runs need `OPENROUTER_API_KEY`; definitions come from the
built-in catalogue (the shipped layer), replaced by name by `--root`
(or a nearby `res/` directory), the project layer. That folder needs a
`bundle.yaml` with `metadata.id: agentsdk.project` listing its agents,
skills and workflows (see [agentdef](agentdef.md)); a missing folder
is skipped, one without `bundle.yaml` is an error. Query commands are read-only over a SQLite trace; IDs
may be unique prefixes. `step --context` prints the exact rebuilt
request; `replay --print` shows the body that would be resent.
