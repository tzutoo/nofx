# Signal Matrix Terminal Cadence: Plan

Date: 2026-08-16 · Branch context: `feat/self-hosted-signal-stack`

## Goal

Align the three terminal panels that proxy the free, self-hosted signal service (Signal Matrix, Flow, Liquidation Map) with the service's actual ingest cadence — replacing the stale 5-minute poll and its "paid x402 / conserve claw402 funds" justification with a single, tunable named constant defaulting to **3m**, and correcting the stale comments at the touched sites.

## Summary

The three panels currently poll every 5m (`refreshInterval: 300000`) under a justification that they are paid x402 endpoints. They are **not** — the backend proxy passes no wallet key, so every call already routes to the self-hosted signal service (`service/signal`), which re-ranks every `SIGNAL_SERVICE_INTERVAL` (default **3m**). This change introduces one named constant (`VERGEX_TERMINAL_REFRESH_MS = 180_000`) referenced by all three poll sites and rewrites the stale comments. Purely a frontend timing + comment edit: no backend, persistence, API-shape, or demo-mode data changes.

## Current-state analysis

**End-to-end data flow (identical for all three panels):** SWR hook → `api.get…` (`web/src/lib/api/data.ts`) → `httpClient` under `API_BASE='/api'` (`web/src/lib/api/helpers.ts:4`) → backend Gin proxy (`api/handler_vergex.go`) → `newVergexClientForRequest` (`:112-123`, builds `vergex.NewClient("", "", logger)` with **no wallet key**) → `provider/vergex` client → self-hosted `service/signal` over `SIGNAL_SERVICE_BASE_URL` (default `:8480`; `provider/vergex/client.go:30` baseURL comment: *"self-hosted signal service (ranking / lab / netflow / heatmap)"*; `client.go:82` notes the wallet key is *"no longer used by this client"*). Each ingest cycle swaps the snapshot and re-ranks → source fresh every 3m (`service/signal/config.go:71`).

**The three 5m poll sites:**
1. Flow panel — `TerminalDashboard.tsx:187-193` `realFlow` SWR, `refreshInterval: 300000`, stale comment (2 lines, incl. the client-side topology-animation note) → `api.getFlowMarkets` (`data.ts:364`) → `/api/vergex/flow-markets` → `handleVergexFlowMarkets` (`handler_vergex.go:86`).
2. Signal Matrix — `TerminalDashboard.tsx:194-199` `realSignalRank` SWR, `refreshInterval: 300000`, stale comment → `api.getSignalRanking` (`data.ts:381`) → `/api/vergex/signal-ranking` → `handleVergexSignalRanking` → self-hosted `Rank()`.
3. Liquidation Map — `LiquidationMap.tsx:60` `opts` object (`refreshInterval: 300000`) **shared** by the `primary`/`alt` SWR hooks → `api.getVergexCostLiquidationHeatmap` (`data.ts:250`) → `/api/vergex/cost-liquidation-heatmap`. Stale header comment at `LiquidationMap.tsx:26-30`.

**Verified seams (do not re-check):**
- `FlowMarkets.tsx` and `SignalMatrix.tsx` are pure props-render components (no `refreshInterval`). `LiquidationMap.tsx` polls independently via the shared `opts`.
- No named SWR refresh-constant exists anywhere in `web/src` — this introduces the first.
- `web/src/constants/` holds only `branding.ts` (brand links) — unrelated, not the home.
- `api.getVergexSignalRanking` (data.ts:230) and `api.getVergexSignalLab` are **not** used by these poll sites; do not confuse them with `getSignalRanking`.
- `KlineChart.tsx:46` uses `refreshInterval: 60000` (K-line) — **not** one of the three sites; leave unchanged.

## Design

### New constant — `web/src/components/terminal/constants.ts`

```ts
// Refresh cadence (ms) for the vergex terminal panels (Signal Matrix, Flow,
// Liquidation Map). These are served by the FREE self-hosted signal service
// (SIGNAL_SERVICE_BASE_URL, default :8480) and re-ranked each
// SIGNAL_SERVICE_INTERVAL cycle (default 3m). Tune alongside the service
// interval so polls never outpace a fresh snapshot.
export const VERGEX_TERMINAL_REFRESH_MS = 180_000
```

**Why terminal-local** (vs. `web/src/constants/`, which is brand-owned/integrity-tagged; vs. `lib/api/`, which holds functions/types not timing): both consumers are siblings in `web/src/components/terminal/`, giving clean `./constants` relative imports and placing the knob next to the panels that use it. Future terminal panels import the same file.

**Value / naming:** `VERGEX_TERMINAL_REFRESH_MS = 180_000` (valid TS numeric separator for ES2021+ target). Comment ties it to `SIGNAL_SERVICE_INTERVAL` for future tuning.

**Cadence / staleness tradeoff (decision kept at 3m, rationale documented):**
The two timers (SWR `refreshInterval`, counted from hook mount, and the ingest worker, counted from service start) are independently phased. Polling at exactly 3m with a 3m ingest can land just before a snapshot swap and show ~3m-old data for that cycle. This is the locked default; the tradeoff is documented here so a future tune can move the value down to `90_000`–`120_000` (sub-3m) to guarantee catching every fresh snapshot at negligible cost, or stay at 3m and accept a staleness bound of ≈ one ingest period. The chosen constant makes either a one-line change.

### Reference-site changes

**`TerminalDashboard.tsx`** — add `import { VERGEX_TERMINAL_REFRESH_MS } from './constants'`; replace `refreshInterval: 300000` with `refreshInterval: VERGEX_TERMINAL_REFRESH_MS` in both `realFlow` (`:187-193`) and `realSignalRank` (`:194-199`).
- `realFlow` comment → `// self-hosted signal service — refresh aligns to SIGNAL_SERVICE_INTERVAL (3m); the topology beam animation is client-side and stays fast regardless`
- `realSignalRank` comment → `// self-hosted signal service — refresh aligns to SIGNAL_SERVICE_INTERVAL (3m)`

**`LiquidationMap.tsx`** — add the `./constants` import; replace `refreshInterval: 300000` inside the **single shared `opts`** (`:60`) with `refreshInterval: VERGEX_TERMINAL_REFRESH_MS`. Because `opts` is shared, both `primary` and `alt` hooks inherit the cadence with no change to the fallback logic (`needAlt`, `!primary.isLoading`, `keepPreviousData: true`).
- Header comment (`:26-30`) → drop the "paid"/wallet/5m claim; e.g. *"Served by the free self-hosted signal service; refresh aligned to its 3m ingest interval (SIGNAL_SERVICE_INTERVAL)."* The `hip3_perp`-only market-coverage sentence is out of scope (predates the crypto-major heatmap fallback) — leave it.

### State / data flow

No state-shape changes: SWR keys (`['flow-markets', traderId]`, `['signal-rank', traderId]`, `['heatmap', marketType, symbol]`), fetchers, response shapes, and derived props are untouched — only the `refreshInterval` atom changes. Consecutive polls may return identical snapshots (data unchanged between cycles) — expected, harmless, and `keepPreviousData:true` keeps stale bins rendered during refetch.

**Demo mode (useDemoEngine):** the constant is referenced only inside the real SWR option objects. When demo `on` is set, the matrix/flow hooks still mount and fetch in the background (keys depend only on `traderId`; only `LiquidationMap`'s fetcher is bypassed by `demo`), so the new cadence accelerates those discarded background hits from 5m→3m. No user-visible change to the demo display; documented for completeness.

### Error handling / edge cases

- Stale/identical data between cycles: expected; SWR keys unchanged, `shouldRetryOnError:false`/`keepPreviousData` behavior identical.
- Service down/slow: cadence change doesn't alter request error paths (proxied through `httpClient`/`handleError`/`vergexErrorStatus`); faster polling surfaces an outage sooner, same degraded rendering.
- LiquidationMap `primary`/`alt` fallback: shared `opts` uses the constant; fallback timing matches primary.
- Multiple terminal instances: no shared mutable state; constant is immutable, module-scoped.

## File-by-file impact

| File | Change | Depends on |
|------|--------|-----------|
| `web/src/components/terminal/constants.ts` | **Add** — `VERGEX_TERMINAL_REFRESH_MS = 180_000` + comment | none |
| `web/src/components/terminal/TerminalDashboard.tsx` | **Modify** — import constant; set `refreshInterval: VERGEX_TERMINAL_REFRESH_MS` in `realFlow` + `realSignalRank`; rewrite two comments | `constants.ts` |
| `web/src/components/terminal/LiquidationMap.tsx` | **Modify** — import constant; replace `300000` in shared `opts`; rewrite header comment | `constants.ts` |
| `FlowMarkets.tsx`, `SignalMatrix.tsx` | No change (props-only) | — |
| `web/src/lib/api/*`, backend, signal-service | No change | — |

**Ordering:** `constants.ts` first (must exist for imports to compile); then both component edits. All three must land in the same change to leave the tree compiling.

## Risks & migration

- **Non-breaking, additive**: no interface/persistence/API changes, no backend rollout, no migration. Revert = restore the literal + comments.
- **Load increase**: 3 polls every 5m → 3m (≈67% more requests on a trivial in-memory service) — negligible.
- **Comment-drift risk**: constant comment references `SIGNAL_SERVICE_INTERVAL`; a separate retune is a manual hint only. Acceptable; no cross-repo wireup.
- **Out-of-scope staleness enumerated (do NOT act on now):**
  - Backend stale comments: `api/handler_vergex.go:80` (`handleVergexFlowMarkets` doc — "paid x402 endpoint … claw402 wallet") and `handler_vergex.go:120-121` (`newVergexClientForRequest` — "Pass the caller's claw402 wallet key if … routes the heatmap to claw402 only when a key is present"), both contradicted by `vergex.NewClient("", "", …)` + `client.go` "walletKeyHex … no longer used".
  - Frontend empty-state strings still referencing claw402 despite the self-hosted source: `SignalMatrix.tsx` `"No signal data (claw402)."`, `FlowMarkets.tsx` `"(claw402 payment required)."`, `TerminalDashboard.tsx` `"Deposit Base USDC to the Claw402 wallet…"` and its `/claw402/i` error classifier.
  - These belong to the deferred claw402-removal work, not this cadence change.

## Implementation order

1. Create `web/src/components/terminal/constants.ts` (atomic with step 2 — consumers' imports won't resolve without it).
2. Edit `TerminalDashboard.tsx` and `LiquidationMap.tsx` (imports + `refreshInterval` swaps + comment rewrites). Steps 1–2 land together.
3. Verify (below).

## Verification

- `cd web && npm run build` (or the repo's canonical web gate — confirm via `web/package.json` scripts; use `tsc` if the repo defines no build). Must pass; the numeric-separator literal and new import must type-check.
- `grep -rn "300000" web/src` — confirm the three sites are cleared and no other `refreshInterval: 300000` remains (KlineChart's `60000` and other literals unaffected).
- `grep -rn "VERGEX_TERMINAL_REFRESH_MS" web/src/components/terminal` — confirm `constants.ts` defines it and both consumers import it.
- Manual: run the signal service on `:8480`, start the frontend, watch the network tab for `/vergex/signal-ranking`, `/vergex/flow-markets`, `/vergex/cost-liquidation-heatmap` firing every ~3m; confirm identical payloads between cycles render unchanged (no flicker/jump); confirm demo mode (Shift+D) is unaffected.
- Unknowns to validate during implementation: no other file references `300000` for these panels (grep covers); TS toolchain accepts `180_000` (fallback `180000`); which command is the canonical web gate.

## References

- `api/handler_vergex.go:112-123` — `newVergexClientForRequest` (no wallet key → self-hosted)
- `provider/vergex/client.go:30,82` — baseURL comment; wallet key no longer used
- `service/signal/config.go:71` — `SIGNAL_SERVICE_INTERVAL` default 3m
- `web/src/components/terminal/TerminalDashboard.tsx:187-199` — matrix + flow SWR polls
- `web/src/components/terminal/LiquidationMap.tsx:60,26-30` — heatmap poll + header comment
- `web/src/lib/api/data.ts:364,381,250,45-55` — API fns + types; `helpers.ts:4` `API_BASE`
- `docs/plans/self-built-signal-stack-2026-08-14.md` — prior art
