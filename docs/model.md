# model

The provider seam and the shared vocabulary: `Item` (one transcript
entry), `Tool`, `ToolResult`, `Request`/`Response`/`Usage`, `Delta`
(streaming), `Provider` (`Sample` plus `Model`), `FilterSpec` (tool
filters: include/exclude globs, max-effect cap, allow list), and the
coded tool-error envelope (`model.Errorf`, `Code*`) whose JSON shape
matches the capability registry's, so the model sees one vocabulary.

`Sample` streams deltas but the response item is complete only when
it returns; providers must not hand back partial tool calls as final.
Usage carries token counts and `CostUSD` (0 when a provider does not
price). `CallerInfo` mirrors the registry `Caller` without importing
it; `host` converts at the registry boundary.
