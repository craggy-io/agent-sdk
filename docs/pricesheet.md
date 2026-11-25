# pricesheet

The Craggy price sheet: which model sits behind each tier, and what models
and cloud resources cost. Services ask for a tier, never a model ID; the
operator picks the model behind each tier in craggy-manager, and a job
publishes the choice with current prices as one signed file at
`https://prices.craggy.io/v1/sheet.json` (craggy-manager
`docs/model-tiers.md` has the whole design).

| Slot | Meaning |
| --- | --- |
| `low`, `med`, `high` | Tiers, cheapest first. The operator swaps their models at will. |
| `decision` | Checked decisions made for a subsystem or pipeline. |
| `audio` | The default text-to-speech model for voice replies. |
| `embed` | The embedding model. Pinned: vectors from different models can't be compared. |

```go
prices := &pricesheet.Reader{CacheFile: filepath.Join(stateDir, "prices.json")}

sheet := prices.Current(ctx)        // re-fetches when older than an hour
m := sheet.Tier(pricesheet.Low)     // m.ID, m.InUSDPerM, m.OutUSDPerM, m.MaxOutput
cpu, _ := sheet.Cost("gcp.run.vcpu_second")

cfg := config.Defaults()
sheet.Profiles(&cfg)                // fast = low, default = med, deep = high
```

- **Always a sheet.** Before the first fetch, and whenever nothing better is
  held, a Reader holds `Builtin()`, compiled into this module (version 0). A
  failed fetch keeps what is held and shows in `Status()`. Older signed sheets
  without `decision` and `audio` use the built-in choices until republished.
- **Signed.** A fetched file is used only if its Ed25519 signature checks
  against `TrustedRoots()`. The signature covers `craggy/pricesheet/v1\n` and
  the sheet's exact bytes, so it can't be confused with an event-log head.
- **Never backwards.** A sheet with a lower `version` than the one held is
  refused, so a stale mirror can't roll a tier back.
- **No background work.** `Current` fetches inside the call, bounded by
  `Timeout` (five seconds). A Cloud Run host refreshes during a request.
- **Costs, not charges.** Prices are what things cost Craggy, in US dollars.
  `fx.gbp` is the dollars one pound buys. What a customer is charged stays in
  auth-services and the app.

`Sign` is for the publisher (craggy-manager's prices job), which holds the
signing key.
