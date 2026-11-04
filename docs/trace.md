# trace

Append-only events plus content-addressed blobs behind one `Sink`
interface, with a `Recorder` that stamps session/run/node/step
metadata. Sinks: `trace/memory` (tests, ephemeral hosts) and SQLite
in the `adapters/tracesqlite` nested module (durable; one writer via
a lease). `trace/sinktest` is the shared conformance suite every sink
must pass.

`trace/query` reads it back: runs and node trees, per-step events,
usage rollups, event filters, and `RebuildRequest` — the exact
provider request body for a step, rebuilt byte for byte from the
recorded item hashes and params blob. The same trace is the workflow
journal and the host's crash-recovery source; downstream analytics
should consume events, not internal state.
