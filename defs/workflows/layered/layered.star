# layered: plan, review in parallel, synthesise.
WORKFLOW = workflow(
    description = "Plan, review in parallel, synthesise.",
    input = {"message": schema.string(required = True)},
    agents = ["assistant", "reviewer"],  # optional allowlist (globs)
    profiles = {"deep": "default"},      # optional remap for this subtree
    limits = {"usd": 1.0},               # optional, can only tighten
)

def run(input):
    plan = agent("assistant", task = "Plan this task:\n" + input.message, phase = "plan")

    reviews = parallel([
        spec("reviewer", task = "Check correctness:\n" + plan.text),
        spec("reviewer", task = "Find missing cases:\n" + plan.text),
    ], phase = "review")

    return agent("assistant", phase = "synthesise", task = render(
        "Task:\n{t}\n\nPlan:\n{p}\n\nReviews:\n{r}",
        t = input.message,
        p = plan.text,
        r = "\n\n".join([r.text for r in reviews]),
    ))
