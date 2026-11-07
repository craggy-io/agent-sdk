# agent-turn: the implicit workflow behind a single RunAgent. Runs one
# agent on one message. Used explicitly when a host wants the
# single-agent turn journaled as a workflow.
WORKFLOW = workflow(
    description = "Run one agent on one message.",
    input = {
        "message": schema.string(required = True),
        "agent": schema.string(),
    },
)

def run(input):
    name = getattr(input, "agent", None) or "assistant"
    return agent(name, task = input.message, phase = "turn")
