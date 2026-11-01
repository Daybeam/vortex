// Package ha reserves the contracts for running the orchestrator hub as a
// primary/standby pair instead of a strict singleton.
//
// Nothing here is wired into the runtime yet: no main*.go imports this package
// and no implementation exists. These signatures are committed ahead of the
// implementation (docs/plan/HUB_HA_PRIMARY_STANDBY_DESIGN.md) so that call sites
// can be introduced phase by phase without reshaping the contract later.
//
// Invariants the future implementations must preserve:
//
//   - Single writer: only the lease holder advances task graphs, runs cron and
//     tickers, and executes chat turns. Standbys serve reads.
//   - Fencing over self-belief: every mutation carries the Fence minted with the
//     lease that authorized it, and stores reject a Fence older than the highest
//     one they have accepted. A paused old leader cannot write after a failover.
//   - Leadership is proven by a renewable Lease evaluated against the backend's
//     clock, never by the hub's local clock alone.
//   - Adoption is at-least-once: a step that was running when the previous leader
//     died is retried; completed steps are never re-executed.
package ha
