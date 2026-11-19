# agentdef

YAML and Starlark agent definitions, skills and workflows, loaded as bundles by
foundation-kit's `bundle` package and compiled by script-services'
shared compiler (`starcompile`).

## Bundles

Definitions live in bundles: folders with a `bundle.yaml` that declares
one `application` or lists agents, skills and workflows as contributions. **A folder needs a
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

A bundle with one workflow can declare its display description at the top
level and use `application: {id: ..., kind: agentsdk.workflow@1, path: ...}`.
The catalogued workflow carries that description into its pinned trace.

| Kind | File | Value |
| --- | --- | --- |
| `agentsdk.agent@1` (`AgentKind`) | `.star` calling `agent(...)` exactly once; `name` must equal the contribution `id` | `*Definition` |
| `agentsdk.skill@1` (`SkillKind`) | `SKILL.md`: front matter (`name`, `description`, optional `for_tools`) and a body fetched on demand; a `name` must equal the `id` | `*Skill` |
| `agentsdk.workflow@1` (`WorkflowKind`) | workflow `.star`, checked at load, run by package `workflow` | `*Workflow` |
| `agentsdk.slot-filler@1` (`SlotFillerKind`) | YAML: what fills a slot an agent seeks (see "Slots") | `*Filler` |

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
delegates, workflows, seeks, loop)` where `tools` is a
`tools_filter(include, exclude, max_effect, allow, inherit)` and
`loop` a `loop_spec(max_steps, completion, output, ...)`; `output`
accepts `schema.*` values, raw schema dicts, JSON strings or a schema
filename. Prompt entries: `"text"`, `{"name": "text"}`,
`{"file": "x.md"}` (contents read at load, fragment named by file
stem), `{"replace": "name", "text"/"file": ...}`, `{"remove":
"name"}`; a dict prompt appends named fragments in order. Skills come
only from `SKILL.md` contributions.

A YAML agent keeps progressive tool disclosure with its tool grant:

```yaml
tools:
  include: ["*"]
  exclude: []
  progressive: true
  always_loaded: [skill, result_read]
```

Omitting `tools`, or using `tools: {}`, inherits every tool already exposed
by the runtime. `progressive` defaults to false. `always_loaded` contains
tool-name patterns and maps to the loop's pinned tools. It can keep a
permitted grouped tool in the initial request, but cannot restore an excluded
or runtime-absent tool. Starlark definitions express the same policy with
`loop_spec(progressive_tools = True, pinned_tools = [...])`.

`Resolve` applies inheritance (design §4) from one catalog and renders
the system prompt with `{{.Agent.Name}}`, `{{.Date}}` and
`{{.Vars.*}}` templates (`missingkey=error`). `ResolveDynamic`
resolves a runtime definition, which must extend a catalogued one.
`ParseSource` compiles a runtime source declaring exactly one agent
without cataloguing it (the runtime-registration path); it has no
bundle, so nothing to `load()` and no files to read.

## Skills for tools

A skill can say which tools it explains, so that any agent able to call
one of them carries it without naming it. A subsystem ships the skill
in the same bundle as its tools:

```markdown
---
name: boards
description: How to lay out and solve an investigation board.
for_tools: [boards.*, boards_*]
---
Place the evidence first, then ...
```

`for_tools` is one tool-name pattern or a flow list of them, matched as
tool filters match names; `*` alone is refused. `Resolve` captures the
catalogued skills with `for_tools` the agent does not already carry, and
`Resolved.AttachToolSkills(names)` adds those matching one of `names` to
`Skills`, `SkillBodies` and the prompt's skills list, recording
`Provenance["skills.<name>"] = "tool <first match>"`. `host` calls it
with the node's narrowed tool view (host view ∩ definition filter ∩
call-site filter), so an agent whose filter leaves a tool out is never
taught it. `WithoutSkills` drops these too.

## Slots

An agent says what it wants by the name of a **slot**, not by the bundle
that provides it: `seeks = ["memory", seek("search", optional = True,
prefer = "craggy-memory")]`. A plain name is needed; `seek()` can make
it optional and name the filler the agent prefers when it is installed.
Seeks merge along the chain: a later definition's seek of a slot
replaces an earlier one's. A slot is lower-case words joined by hyphens.

What fills a slot is an `agentsdk.slot-filler@1` contribution of the
bundle that provides it, a YAML file:

```yaml
slot: memory
title: Agent Memory (Basic)   # what a person chooses it by
description: One line about it.
priority: 10                  # highest wins when nothing else decides
tools: [memory_*]             # the tool names it brings; may end in *
prompt: activation.md         # how to use them, beside this file
skills: [memory-notebook]     # skills it brings, from any bundle
seed: memory.seed             # optional: a capability the host calls when a conversation starts
```

It must bring tools, a prompt or skills. `Fillers(cat, slot)` (and
`Loader.Fillers`) lists a slot's fillers, highest priority first, then by
name. Choosing one is the host's business: this package only ranks them.

`ResolveFilled(name, vars, fills)` (and `ResolveDynamicFilled`) resolves
with slots filled. For each `Fill{Slot, Filler}`: the filler's prompt is
a fragment `slot.<slot>` after every fragment the chain wrote, rendered
as a template like the others; its skills join the agent's catalogue;
its tools are listed in `Resolved.Pinned`, which `host` hands the loop
as `PinnedTools` so progressive loading never defers them; and the tools
of every other filler of the same slot are added to the filter's
`exclude`. `Resolved.Filled` records what was filled, and
`Provenance["slot.<slot>"]` the filler. `Resolve` is `ResolveFilled`
with none.

## Workflow-owned agents

A chat bundle declares its workflow, agents and skills as named contributions.
A YAML agent declares its `name`, `prompt`, `tools`, `skills` and a relative
`loop` file. A child agent can inherit the loop and skills using `extends`.
Declare a bundle dependency when the parent belongs to a different bundle.
The manifest locates each skill at `skills/<name>/SKILL.md`. The loop file and
prompt files are support files relative to their agent YAML.
