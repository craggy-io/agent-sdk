# Shared triggers

A trigger is a declarative condition on an event, independent of the execution target. Foundation Kit owns the event envelope, predicates, field mapping, scoped deduplication, timer syntax and durable admission journal. Script Services compiles the same language for workflows and collectors. Hosts validate producer declarations and own execution, authorization and lifecycle.

```python
WORKFLOW = workflow(
    input={"subject": schema.string(required=True)},
    triggers=[on_event("artifacts.created@1",
        where=ends_with("data.name", ".so"),
        input={"subject": event_field("data.id")},
        deduplicate=["data.id", "data.digest"])],
)
def run(input):
    return {"subject": input.subject}
```

`eq`, `ends_with`, `exists`, `all_of`, `any_of` and `not_` build structured predicates; `event_field` selects an envelope or payload field. Literal input values are JSON. `manual=True` holds admissions for release; `replay=True` explicitly matches retained history. Without explicit deduplication, event publisher, scope and ID identify the occurrence. Workflow declarations also accept `every("5m")` and `cron("0 * * * *")`; cron is UTC and missed ticks coalesce.

Independent JSON contributions use `agentsdk.workflow-binding@1` with `workflow` and `trigger` properties. Workflow compilation validates mapped input names and required fields. Hosts validate declared event paths before activating the catalog. Runtime validation checks actual mapped input values.

A durable admission pins event, mapped input and target revision. An explicit retry creates a new attempt against the current target revision. Interrupted workflows resume the existing workflow journal; external side effects are governed by that journal's reconciliation rules. Collection runs require explicit retry after interruption. This is not a guarantee of exactly-once external effects.
