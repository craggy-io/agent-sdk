# transcript

The append-only conversation: `Transcript.Append` assigns `Seq` and
deep-copies; items are immutable afterwards. `Normalise` is the pure
projection the loop samples from (summaries replace covered ranges,
cleared results become markers, tool groups stay contiguous).
`Check` verifies the invariants (design §13.1) and is what
`agenttest.CheckTranscript` runs in tests. The transcript is the
agent's memory of the conversation; durable cross-run memory is
package `memory`.
