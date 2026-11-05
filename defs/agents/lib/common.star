# Helpers the shipped agent files share. A support file: bundle.yaml does
# not declare it, the agent files load() it. Modules see tools_filter,
# loop_spec and schema, but not agent().

def fragment(name, text):
    """One named prompt fragment, as a prompt list entry."""
    return {name: text}

# The verdict-and-findings shape a structured review returns.
REVIEW_OUTPUT = {
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
}
