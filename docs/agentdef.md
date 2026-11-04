# agentdef

Starlark agent definitions. A catalogue root is any `fs.FS` (or
directory via `DirRoots`) with:

- `agents/*.star` — top level calls `agent(...)` and may call
  `skill(...)`; `load()` works within the root.
- `skills/<name>/SKILL.md` — front matter (`name`, `description`)
  plus a body fetched on demand.
- `workflows/<name>.star` — catalogued for package `workflow`.

`agent(name, extends, description, profile, prompt, tools, skills,
delegates, workflows, loop)` where `tools` is a
`tools_filter(include, exclude, max_effect, allow, inherit)` and
`loop` a `loop_spec(max_steps, completion, output, ...)`; `output`
accepts `schema.*` values, raw schema dicts, JSON strings or a schema
filename. Prompt entries: `"text"`, `{"name": "text"}`,
`{"file": "x.md"}` (contents read at load, fragment named by file
stem), `{"replace": "name", "text"/"file": ...}`, `{"remove":
"name"}`; a dict prompt appends named fragments in order.

Roots shadow by name (later wins, recorded in `Loader.Shadowed`).
`Load` swaps catalogues atomically; `ReloadIfChanged` re-reads when
content changed. `Resolve` applies inheritance (design §4) and
renders the system prompt with `{{.Agent.Name}}`, `{{.Date}}` and
`{{.Vars.*}}` templates (`missingkey=error`). `ResolveDynamic`
resolves a runtime definition, which must extend a loaded one.
`ParseSource` validates an in-memory source without cataloguing it
(the runtime-registration path).
