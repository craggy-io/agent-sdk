# assistant: interactive work, delegating review to the reviewer.
load("@agentsdk.shared.common/common.star", "fragment")

agent(
    name = "assistant",
    extends = "base",
    description = "General assistant for interactive work.",
    prompt = [
        fragment("assistant", "Help with the user's task directly. Delegate review work to the reviewer when a second pair of eyes would help."),
    ],
    delegates = ["reviewer"],
)
