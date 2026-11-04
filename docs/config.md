# config

Session configuration as Go values: `SessionConfig{Profiles, Limits,
Loop, Input, Events}`, `Defaults()` (profiles `default`/`fast`/`deep`,
provider data collection denied), and `Resolve(base, layers, want)`
implementing the layering rules of design §3 — limits minimise, loop
fields last-wins, profiles remap then resolve with fallback to
`default`. `ParseSize` accepts `32KiB`-style sizes.

There is deliberately no file format here. Hosts that keep config in
YAML use the `adapters/configyaml` nested module (`Load`/`Parse`
overlay a document onto `Defaults`, unknown fields rejected); see
`examples/host/session.example.yaml`. Secrets never belong in config
files: providers take keys at construction.
