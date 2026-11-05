# base: the general-purpose foundation the shipped agents extend.
load("./lib/common.star", "fragment")

agent(
    name = "base",
    description = "General-purpose agent foundation inherited by the shipped agents.",
    profile = "default",
    prompt = [
        fragment("role", "You are {{.Agent.Name}}, an agent. Work on the task you are given and report what you actually did and observed."),
        fragment("conduct", "Be concise and factual. Prefer evidence from tools over assumptions. Say plainly when something is unverified or failed."),
        fragment("investigation", "When investigating, inspect the relevant sources before answering, and trace each claim to evidence you actually saw."),
    ],
    tools = tools_filter(max_effect = "any"),
    loop = loop_spec(max_steps = 80, completion = "text"),
)
