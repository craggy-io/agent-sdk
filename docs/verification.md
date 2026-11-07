# Verification

There is no CI workflow yet; the gate below is run locally. All Craggy
dependencies are public modules, so a hosted runner can run the same
gate.

The gate is `gofmt` clean, `go vet`, `go build`,
`go test` and `go test -race` over the core module, plus `go test`
in each nested module (`adapters/configyaml`,
`adapters/tracesqlite`, `cmd/craggy-agent`, `examples/host` builds).

How behaviour is pinned:

- The loop, workflow journal, ask gates, recovery and host control
  run against `agenttest` scripted/chaos providers — no network.
- Trace sinks share the `trace/sinktest` conformance suite; request
  rebuild is asserted byte-equal against a hand-built body.
- The shipped `defs` workflows run end to end in
  `workflow.TestShippedResWorkflows`.
- The capability bridge is tested through real registry invocation
  and through `starlarkcap.Exec`, including a define→promote
  round-trip and refusal without approval.
- `examples/host` is smoke-tested by hand (HTTP listing, memory
  put/get) when its wiring changes.
- Live provider tests are opt-in only (`AGENT_SDK_LIVE=1` with a
  key); CI never needs a secret.
