# Execute one ordinary chat turn on the selected agent.
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
