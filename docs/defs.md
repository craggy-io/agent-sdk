# defs

The shipped definition layer, embedded in the binary. `defs.Sources()`
returns one `bundle.Source` per authored bundle; pass it as
`agentdef.Options.Shipped`. User and project bundles replace these by
kind and name (see [agentdef](agentdef.md#layers)).

## Layout

Each agent, skill and workflow is its own bundle with its own
`bundle.yaml`:

| Folder | Bundle ID | Contributes |
| --- | --- | --- |
| `definitions/base/` | `agentsdk.shipped.agent.base` | `base`: the root every other agent extends |
| `definitions/assistant/` | `agentsdk.shipped.agent.assistant` | `assistant`: interactive work, may delegate to `reviewer` |
| `definitions/reviewer/` | `agentsdk.shipped.agent.reviewer` | `reviewer`: evidence-based review with a structured verdict (profile `deep`) |
| `skills/example-review/` | `agentsdk.shipped.skill.example-review` | `example-review`: a sample `SKILL.md` |
| `workflows/agent-turn/` | `agentsdk.shipped.workflow.agent-turn` | `agent-turn`: one agent on one message, journaled |
| `workflows/layered/` | `agentsdk.shipped.workflow.layered` | `layered`: plan, review in parallel, synthesise |
| `shared/common/` | `agentsdk.shared.common` | no contributions; helper Starlark loaded as `@agentsdk.shared.common/common.star` |

Bundles that use the shared helpers declare
`dependencies: [{id: agentsdk.shared.common}]` in their manifest.

## Replacing a shipped definition

Put a bundle in the user or project layer that contributes the same
`id` and kind, for example a project `reviewer`:

```yaml
apiVersion: craggy.io/bundle/v1
metadata: {id: myproject.reviewer, version: 1.0.0}
contributions:
  - {id: reviewer, kind: agentsdk.agent@1, path: reviewer.star}
```

The loader reports each replacement through `Catalog().Overrides()` and
an `agentdef.shadowed` event.

`defs.TestBuiltinCatalogueResolves` loads and resolves the shipped set, and
`workflow.TestShippedResWorkflows` runs the shipped workflows end to end.
