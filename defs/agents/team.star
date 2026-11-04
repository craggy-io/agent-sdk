# Built-in agent definitions shipped with the SDK. Hosts catalogue them
# as the earliest root; user and project roots shadow them by name.

agent(
    name = "base",
    description = "General-purpose agent foundation inherited by the shipped agents.",
    profile = "default",
    prompt = [
        {"role": "You are {{.Agent.Name}}, an agent. Work on the task you are given and report what you actually did and observed."},
        {"conduct": "Be concise and factual. Prefer evidence from tools over assumptions. Say plainly when something is unverified or failed."},
        {"investigation": "When investigating, inspect the relevant sources before answering, and trace each claim to evidence you actually saw."},
    ],
    tools = tools_filter(max_effect = "any"),
    loop = loop_spec(max_steps = 80, completion = "text"),
)

agent(
    name = "assistant",
    extends = "base",
    description = "General assistant for interactive work.",
    prompt = [
        {"assistant": "Help with the user's task directly. Delegate review work to the reviewer when a second pair of eyes would help."},
    ],
    delegates = ["reviewer"],
)

agent(
    name = "reviewer",
    extends = "base",
    description = "Reviews a proposal for correctness, missing cases and unsupported claims.",
    profile = "deep",
    prompt = [
        {"review": "Review the material you are given. For each finding, state the claim, the evidence for or against it, and a severity. End with a verdict."},
        {"replace": "conduct", "text": "Be strictly evidence-based. Do not approve a claim you cannot point to evidence for; mark it unsupported instead."},
        {"remove": "investigation"},
    ],
    tools = tools_filter(max_effect = "read-only"),
    skills = ["example-review"],
    loop = loop_spec(
        max_steps = 40,
        completion = "structured",
        output = {
            "type": "object",
            "properties": {
                "verdict": {"type": "string", "enum": ["approve", "changes", "reject"]},
                "findings": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "claim": {"type": "string"},
                            "severity": {"type": "string"},
                            "evidence": {"type": "string"},
                        },
                        "required": ["claim", "severity"],
                    },
                },
            },
            "required": ["verdict", "findings"],
        },
    ),
)
