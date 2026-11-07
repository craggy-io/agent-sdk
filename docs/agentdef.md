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
(`./lib/common.star` relative to the file, or `//lib/common.star` from the
bundle root) and, as
`@<dependency-id>/path`, by bundles its `bundle.yaml` lists under
`dependencies` (pass those in `Options.Sources`); the shipped definitions
use `load("@agentsdk.shared.common/common.star", ...)`. Modules an agent file
loads see `tools_filter`, `loop_spec` and `schema`, not `agent`. Prompt
`{"file": ...}` fragments and `loop.output` schema files are read from
the agent file's bundle, relative to the file, or a declared `@bundle.id/path` dependency; a path outside the
bundle, or a symbolic link, fails to load.

## Layers

`Open(ctx, Options)` holds the composed catalog in a `bundle.Live`.
Each layer contains any number of independently identified bundles:

| Layer | Option | Discovery |
| --- | --- | --- |
| shipped | `Shipped` (usually `defs.Sources()`) | explicit `[]bundle.Source` |
| user | `UserDir` | recursively discovered bundle tree |
| project | `Project` | recursively discovered bundle tree |

A directory with `bundle.yaml` is one bundle; otherwise subdirectories are groups.
Missing or empty roots are empty layers. Hidden staging directories are skipped.
Loose `.star` files outside a bundle are errors. A bundle may hold several
contributions (as in the project example above) or just one; the shipped set and
`promote` use one bundle per agent, skill or workflow so each keeps its
prompt/schema/support files beside it. Bundle IDs are unique across layers; they no longer identify an entire layer.

User replaces shipped and project replaces both, by kind and contribution name.
The loader validates each layer through `bundle.Load`, derives explicit
`bundle.Override` entries directly to the winning definition, then validates and
publishes the combined catalog through `bundle.Live`. Duplicate names within a
layer fail. `Catalog().Overrides()` reports the exact replaced and winning refs;
`host.Open` traces these as `agentdef.shadowed` events. `Sources` supplies shared
libraries or extra definitions outside this hierarchy. A name conflict with these
or with Go `Definitions` remains an error.

Shared support belongs in a separate bundle declared under `dependencies`.
Dependencies can be in the same layer or in `Options.Sources` when shared between
layers. Starlark `load()`, prompt `{"file":"@bundle.id/prompt.md"}` and
`loop.output="@bundle.id/schema.json"` all read accepted dependency snapshots.
Paths cannot escape those snapshots; undeclared dependencies are errors.

`Reload` rediscovers directories and replaces the catalog atomically. Failed loads
keep the previous catalog. The host reloads before new runs and traces failures
as `agentdef.reload_failed`. Resolved prompts, schemas and skill bodies come from
one catalog; in-flight runs keep that snapshot. Workflows retain their compiled
program. `AddDefinition` updates Go definitions through the same live catalog.

`InstallAgent` and `InstallWorkflow` (behind `agents.promote`/`workflows.promote`)
use `bundle.Live.Edit` to create or update one user bundle each:
`definitions/agentsdk.user.definitions.<name>/agent.star` or
`workflows/agentsdk.user.workflows.<name>/workflow.star`. Reinstalling preserves
that bundle's identity and replaces its contribution. A conflicting separately
authored user bundle is rejected. Invalid installs roll back. Edit preserves
support assets and dependencies, but rewrites manifest formatting and comments.

Hosts that build their own catalog with `bundle.Load` instead of `Open` use
`SourcesIn(ctx, root)` to discover a layer's bundle tree the same way `Open`
does, and `Compilers()` for the agent, skill and workflow compilers.
`WorkflowBuiltins()` lists the predeclared names workflow files are checked
against, and `ValidateMaxEffect` rejects unknown `max_effect` caps at
definition time.

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
