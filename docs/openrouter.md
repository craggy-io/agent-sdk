# openrouter

The OpenRouter implementation of `model.Provider` (design §8): plain
`net/http` and a small SSE reader, no vendor SDK. It is the only
provider shipped; anything else implements the same two-method
interface (`Sample`, `Model`).

```go
p := openrouter.New(os.Getenv("OPENROUTER_API_KEY"),
    openrouter.WithIdleTimeout(2*time.Minute), // optional
)
```

| Option | Default | Use |
| --- | --- | --- |
| `WithBaseURL(u)` | `DefaultBaseURL` (`https://openrouter.ai/api/v1`) | point at an `httptest` server or a proxy |
| `WithHTTPClient(c)` | `http.DefaultClient` | transport, proxy or TLS settings |
| `WithIdleTimeout(d)` | `DefaultIdleTimeout` (180s) | stream idle timeout when a request sets none; any byte, including SSE keep-alive comments, resets it |

The key is passed in by the host; this package never reads the
environment.

## Behaviour

- `Sample` streams one completion and calls `onDelta` (may be nil) per
  chunk. The request body comes from `BuildRequestBody`, which shares
  `trace/query.BuildBody` with trace replay, so the bytes sent are the
  bytes `trace/query.RebuildRequest` reproduces for that step.
- `Model` reads `GET /models` once per client and answers later lookups
  from that cache (context window and per-token pricing).
- `MapHTTPError` turns a non-200 response into `*model.Error`:
  408/429/502/503 are transient, every OpenRouter "context exceeded"
  form maps to the context-exceeded code, `Retry-After` is carried, and
  the upstream message and provider name are kept for the trace.

Profiles in [config](config.md) carry the OpenRouter routing knobs
(`fallbacks`, `provider.require_parameters`, `provider.data_collection`,
`cache`); definitions name profiles, never models.

## Testing

Unit tests run against `httptest` servers. The live test in
`live_test.go` is skipped unless `OPENROUTER_API_KEY` is set and
`AGENT_SDK_LIVE=1`; `AGENT_SDK_LIVE_MODEL`,
`AGENT_SDK_LIVE_DATA_COLLECTION` and `AGENT_SDK_LIVE_PARALLEL=1` adjust
it. For provider-free tests use [agenttest](agenttest.md).
