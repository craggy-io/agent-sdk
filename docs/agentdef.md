# agentdef

Starlark agent definitions, skills and workflows, loaded as bundles by
foundation-kit's `bundle` package and compiled by script-services'
shared compiler (`starcompile`).

## Bundles

Definitions live in bundles: folders with a `bundle.yaml` that lists
every agent, skill and workflow as a contribution. **A folder needs a
`bundle.yaml`**; nothing is discovered from file names or folder
layout, and files the manifest does not list are only support files
(helpers that definition files `load()`, prompt and schema files).

```yaml
apiVersion: craggy.io/bundle/v1
metadata: {id: agentsdk.project, version: 1.0.0}
contributions:
  - {id: reviewer, kind: agentsdk.agent@1, path: agents/reviewer.star}
  - {id: example-review, kind: agentsdk.skill@1, path: skills/example-review/SKILL.md}
  - {id: layered, kind: agentsdk.workflow@1, path: workflows/layered.star}
```

| Kind | File | Value |
| --- | --- | --- |
| `agentsdk.agent@1` (`AgentKind`) | `.star` calling `agent(...)` exactly once; `name` must equal the contribution `id` | `*Definition` |
| `agentsdk.skill@1` (`SkillKind`) | `SKILL.md`: front matter (`name`, `description`) and a body fetched on demand; a `name` must equal the `id` | `*Skill` |
| `agentsdk.workflow@1` (`WorkflowKind`) | workflow `.star`, checked at load, run by package `workflow` | `*Workflow` |

All three kinds are global: an id names one agent, skill or workflow
across the catalog. Two bundles defining the same id is a
`bundle.id_conflict` error unless one layer replaces the other. One
bundle cannot use one id for two kinds.

`load()` in agent and workflow files is served by the file's own bundle
(`./lib/common.star`, `//agents/lib/common.star`) and, as
`@<dependency-id>/path`, by bundles its `bundle.yaml` lists under
`dependencies` (pass those in `Options.Sources`). Modules an agent file
loads see `tools_filter`, `loop_spec` and `schema`, not `agent`. Prompt
`{"file": ...}` fragments and `loop.output` schema files are read from
the agent file's bundle, relative to the file; a path outside the
bundle, or a symbolic link, fails to load.

## Layers

`Open(ctx, Options)` holds the catalog in a `bundle.Live`. Each layer is
one bundle with a fixed `metadata.id`:

| Layer | Option | Bundle | `metadata.id` |
| --- | --- | --- | --- |
| shipped | `Shipped` (usually `defs.Source()`) | any `bundle.Source` | `agentsdk.shipped` |
| user | `UserDir` | `<UserDir>/agentsdk.user` | `agentsdk.user` |
| project | `Project` | the directory itself | `agentsdk.project` |

User replaces shipped and project replaces user (and shipped), by id,
through a `BundleOverride` chain; `Catalog().Overrides()` lists what
was replaced, and `host.Open` traces
one `agentdef.shadowed` event per replacement. A missing layer folder
is skipped; a folder without `bundle.yaml` is
`bundle.manifest_missing`. Load failures are `*bundle.LoadError`s;
read `Diagnostics` for every problem and its position. `Sources` adds
other bundles (libraries, extra definitions) and `Definitions` adds
Go-built definitions (owner `agentsdk.go`); neither replaces anything,
so a shared name is a conflict.

`Reload` reloads every layer: the catalog is replaced whole or not at
all, so a bad edit keeps the previous catalogue. The host reloads before
resolving each run and traces a failed reload as `agentdef.reload_failed`. `AddDefinition` adds or replaces a Go-built
definition and reloads. `InstallAgent` and `InstallWorkflow` (behind
`agents.promote`/`workflows.promote`) write `agents/<name>.star` /
`workflows/<name>.star` and its `bundle.yaml` entry into the user bundle
with `bundle.Live.Edit` at `<UserDir>/agentsdk.user`, rolled back if the
reload fails. Edit rewrites `bundle.yaml` with foundation-kit's encoder,
so comments in it are not kept. Other folders in `UserDir` are not read.

## Definitions

`agent(name, extends, description, profile, prompt, tools, skills,
delegates, workflows, loop)` where `tools` is a
`tools_filter(include, exclude, max_effect, allow, inherit)` and
`loop` a `loop_spec(max_steps, completion, output, ...)`; `output`
accepts `schema.*` values, raw schema dicts, JSON strings or a schema
filename. Prompt entries: `"text"`, `{"name": "text"}`,
`{"file": "x.md"}` (contents read at load, fragment named by file
stem), `{"replace": "name", "text"/"file": ...}`, `{"remove":
"name"}`; a dict prompt appends named fragments in order. Skills come
only from `SKILL.md` contributions.

`Resolve` applies inheritance (design §4) from one catalog and renders
the system prompt with `{{.Agent.Name}}`, `{{.Date}}` and
`{{.Vars.*}}` templates (`missingkey=error`). `ResolveDynamic`
resolves a runtime definition, which must extend a catalogued one.
`ParseSource` compiles a runtime source declaring exactly one agent
without cataloguing it (the runtime-registration path); it has no
bundle, so nothing to `load()` and no files to read.
