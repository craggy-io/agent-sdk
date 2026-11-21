load("@craggy/flow.star", "flow")

pipeline(
    title = "Project Zero exploitation skills",
    description = "Finds the exploitation techniques in each Project Zero post, merges the same technique across posts and writes a SKILL.md for each.",
    inputs = {"posts": flow.input("project-zero/posts", key = "url", where = "published >= '2019-01-01'")},
    requires = ["core.workflows.run@1", "core.models.complete@1", "core.packs.write@1"],
)

STEPS = [
    flow.read(id = "posts", title = "Project Zero posts since 2019", input = "posts"),
    flow.for_each(
        id = "each_post",
        over = "posts",
        item = "post",
        steps = [
            flow.agent(id = "find", title = "Find techniques", prompt = "prompts/find.md", input = "post", out = "techniques", schema = "schemas/find.json", many = True, budget = 120),
            flow.keep_if(id = "confident", title = "Keep the confident ones", over = "techniques", field = "confidence", op = ">=", value = 0.7),
        ],
    ),
    flow.group(id = "by_technique", title = "Merge the same technique", over = "techniques", by = "slug", out = "skills"),
    flow.for_each(
        id = "each_skill",
        over = "skills",
        item = "skill",
        steps = [
            flow.ask(id = "write", title = "Write SKILL.md", prompt = "prompts/write.md", input = "skill", out = "text", model = "med", max_tokens = 3000),
            flow.save(id = "save", title = "Save to the pack", path = "skills/{skill.slug}/SKILL.md", text = "text"),
        ],
    ),
]

def run(ctx):
    return flow.run(ctx, STEPS)
