// Package pricesheet reads the Craggy price sheet: which model sits behind each
// tier, and what models and cloud resources cost us.
//
// Services ask for a tier (Low, Med, High) or the pinned Embed slot, never a
// model ID. The operator picks the model behind each in craggy-manager; a job
// publishes the choice with current prices as one signed JSON file at URL
// (craggy-manager docs/model-tiers.md).
//
// Invariants:
//   - Nothing in a sheet is used unless its Ed25519 signature checks against a
//     trusted root, or it is Builtin, which is compiled in.
//   - A Reader never goes back: a fetched sheet with a lower Version than the
//     one held is refused, so a stale mirror can't roll a tier back.
//   - A Reader always has a sheet: Builtin until something better is held.
//   - Prices are what things cost Craggy, in US dollars. What a customer is
//     charged is not here.
//
// Concurrency: a Sheet is immutable once returned (its maps are never written
// again). A Reader is safe for concurrent use; Current fetches inside the call,
// never in the background, so a Cloud Run host refreshes during a request.
//
// Exported names: Sheet, Model, Cost, Low, Med, High, Embed, Tiers, Format,
// URL, Sign, Verify, ErrUntrusted, Builtin, TrustedRoots, Reader, Status.
package pricesheet
