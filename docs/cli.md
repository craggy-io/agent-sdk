# craggy-agent CLI

`cmd/craggy-agent` (its own nested module) is the trace debugger and
live smoke runner:

```
craggy-agent [--root <dir>] [--db trace.db|--mem] [--config session.yaml] run (--agent <name>|--workflow <name>) <task...>
craggy-agent --db trace.db resume <workflow-run-id>
craggy-agent --db trace.db runs|tree <run>|step <step> --context|--diff|tools <node>|usage|replay|grep|inputs|recovery|tail|agents
```

Live runs need `OPENROUTER_API_KEY`; definitions come from the
built-in catalogue shadowed by `--root` (or a nearby `res/`
directory). Query commands are read-only over a SQLite trace; IDs
may be unique prefixes. `step --context` prints the exact rebuilt
request; `replay --print` shows the body that would be resent.
