# agenttest

Test providers. `NewScripted(responses...)` replays a queue of
`TextResponse`/`CallsResponse`/errors and records every request, so
loop and workflow behaviour is deterministic without a network.
`ChaosProvider` wraps a provider and injects seeded faults
(truncated streams, transient errors, malformed calls) to prove the
loop's invariants survive them. `CheckTranscript` asserts the
transcript invariants on a run's items. Live smoke tests stay behind
explicit environment gates (`OPENROUTER_API_KEY` plus
`AGENT_SDK_LIVE=1` for the OpenRouter package).
