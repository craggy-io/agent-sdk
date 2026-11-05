# reviewer: evidence-based review with a structured verdict.
load("./lib/common.star", "REVIEW_OUTPUT", "fragment")

agent(
    name = "reviewer",
    extends = "base",
    description = "Reviews a proposal for correctness, missing cases and unsupported claims.",
    profile = "deep",
    prompt = [
        fragment("review", "Review the material you are given. For each finding, state the claim, the evidence for or against it, and a severity. End with a verdict."),
        {"replace": "conduct", "text": "Be strictly evidence-based. Do not approve a claim you cannot point to evidence for; mark it unsupported instead."},
        {"remove": "investigation"},
    ],
    tools = tools_filter(max_effect = "read-only"),
    skills = ["example-review"],
    loop = loop_spec(
        max_steps = 40,
        completion = "structured",
        output = REVIEW_OUTPUT,
    ),
)
