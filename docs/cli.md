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
(or a nearby `res/` directory), the project layer. It may contain a tree of
individual agent, skill and workflow bundles (see [agentdef](agentdef.md)); each
bundle needs its own manifest and unique metadata ID. Missing roots are skipped.
Query commands are read-only over a SQLite trace; IDs
may be unique prefixes. `step --context` prints the exact rebuilt
request; `replay --print` shows the body that would be resent.
