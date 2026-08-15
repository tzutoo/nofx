# Pineify Signal Enrichment: Symbol Selection + AI Decision Data — Plan

## Goal

Leverage Pineify's MCP tools to (1) help select symbols on the signal-matrix path and (2) enrich the AI's decision data (TA, events, rating, flow) beyond what the free self-hosted signal-service provides, filling the free-vs-paid gaps identified in `docs/plans/self-built-signal-stack-2026-08-14.md` §8 and increasing profitability. First map Hyperliquid's tradable universe to Pineify screeners as completely as possible, then integrate Pineify enrichment end-to-end.

## Background

### Hyperliquid universe → Pineify mapping
- Universe enumerated dynamically from `metaAndAssetCtxs` via `provider/hyperliquid/coins.go` `GetPerpDexCoins` (dex `""` = crypto, dex `"xyz"` = TradeFi); ~150+ symbols. Categories: `stock`/`commodity`/`index`/`forex`/`pre_ipo` (`XYZCategory` `coins.go:40-55`) + `crypto`. Classification is **fragmented** across `coins.go XYZCategory`, `handler_klines.go hyperliquidXYZCategory`/`hyperliquidXYZDisplayBase` (`handler_klines.go:241,384`), and hardcoded `StockPerpsSymbols`/`XYZOtherSymbols`/`XYZDisplayNameToCoin` (`kline.go:113,260,286`). `coins.go XYZCategory` is the classifier that actually feeds products (`service.go:123`, `client.go:844`).
- **Pineify-mappable = US-listed `stock` subset ONLY.** Indexes (SP500, XYZ100, SPX, NDX, DJI, VIX...), commodities (GOLD, SILVER, BRENTOIL, CL, COPPER...), forex, pre-IPO (SPCX, OPENAI...), and crypto are NOT mappable. Some `stock`-classified entries are non-US issuers (SMSN, SKHX, LVMH, SONY, TM, RACE, VOW3, BMW, MBG) — questionable for Pineify.
- **Live Pineify tests (performed 2026-08-15):** `get-ai-stock-rating` works for US stocks (NVDA 295/8, NBIS 3379/4, MU 2188/5, SNDK 1273/6) but NOT ETFs/indexes (SPY → none). `get-technical-analysis-snapshot` (300 fields) works for US stocks AND crypto using the **bare ticker** (`ETHUSDT`) — the `BINANCE:` prefix is rejected ("symbol must not include exchange prefix"); plan §2.6's `BINANCE:` usage is outdated. `get-stock-event-context` works (NVDA: 8 events, 1 upcoming earnings). `get-stock-research-snapshot` works (NVDA, MU, SNDK, SKHY returned; CXMT failed). `find-ai-stock-picks`/`find-technical-setups` scan ~500 US symbols but top picks (GAU, BBD, ARIS, TFPM) do NOT align with Hyperliquid candidates. `track-smart-money` persistently unavailable; `find-options-flow-alerts`/`get-market-tide`/`get-sector-flow-snapshot` available for US options/flow.

### Pineify integration seam
- `service/signal/pineify.go` is a stub: only `pineifyEnabled()` (`:14`); header comment locks "calls only inside the single ingest worker, batched, never HTTP request/trading path" (`:3-11`).
- Config `PineifyMCPToken`/`PineifyBaseURL`/`PineifyRatePerMinute` (`service/signal/config.go:22-29`), default base `https://agents.pineify.app/mcp`, rate default 3/min but **user confirmed 30/min max**. `PineifyRatePerMinute` is **UNENFORCED** — no limiter/backoff exists (net-new). Single `http.Client` (`service.go:57`) 30s timeout, no retry.
- Single integration point: `Service.Ingest()` (`service/signal/service.go:74-124`), after the two `ingestDex` calls, gated by `pineifyEnabled()`. Per-symbol data attaches to the `asset` struct (`service.go:16`), reachable via `Snapshot()`.

### AI-prompt consumption seam
- Engine builds `vergex.MarketAnalysis{Ranking, SignalLab, Heatmap}` (`kernel/engine.go:1099` `FetchVergexDataBatch`, `:1215` `populateVergexDetailData`).
- AI sees it via `formatVergexData` (`kernel/engine_prompt.go:1050`) → `vergex.FormatAnalysisForAI` (`provider/vergex/client.go:415`) → `FormatSignalLabMarkdown` (renders `dimensions[]` cap 8 + scalars) / `FormatHeatmapMarkdown`.
- Extension point: `SignalRankItem`/`SignalRankingData` carry `Raw json.RawMessage` (`client.go:51,56`) — exists, not rendered. `MarketAnalysis` has no generic extras field.
- `Dimension` formatter reads `{family,label,direction,strength,percentile,detail}`; free emits 3 rows (POC/24h Momentum/Funding), leaving 5 headroom under cap 8. `percentile` currently renders `-`; `compositeZ`/`score` scalars are empty slots.

### Prior art
- `docs/plans/self-built-signal-stack-2026-08-14.md` §2.6 (Pineify probe), §3.4 (config/rate), §5 (coverage), §8 (paid-vs-free parity, §8.1–8.6), §8.6 (signal-matrix path). Item 7 = "Pineify integration (optional, deferred)".

---

## Gaps analysis — what free data lacks vs paid, and what Pineify fills

This is the field-by-field parity distilled from the self-built-stack plan §8. The rule: only fields the AI **consumes and renders** matter — paid fields the parsers never read (`raw.*`, `cost{state}`, `liquidation{state}`, `meta{asOfBlock}`, `markPriceSource`, `market{isActive,marketId}`) cause **zero prompt regression** when absent. The real risk concentrates in the *values* of the few consumed fields.

| §8 gap | Free product | AI-consumed fields | Verdict | Pineify fills? |
|---|---|---|---|---|
| **R2 §8.2** | `SignalLab()` — 3 proxy rows (POC/24h Momentum/Funding), no percentile, no real technicals | `dimensions[]{family,label,direction,strength,percentile,detail}` | **NOT sufficient** — confirmation gate collapses (POC-direction≈momentum; funding already in ranking) | ✅ **Strong**: `get-technical-analysis-snapshot` (real RSI/EMA/ADX/levels) + `get-stock-event-context` (catalysts) into `dimensions[]`; `percentile`=conviction; emit `compositeZ`/`score` scalars |
| **R3 §8.1** | `Rank()` — per-cohort z (0.45·price24h+0.35·funding+0.20·oi), no crowding/fragility factor | `bias`, `score` (`|score|≥0.4` gate), `category` | **Sufficient** but no crowding/leverage/cascade dimension | ⚠ **Partial**: `get-ai-stock-rating` + `get-stock-research-snapshot` overlay (quality/fragility) |
| **R1 §8.3** | `Heatmap()` — synthetic `longLiq/shortLiq` (exp-taper, 0.33×mark cost) | `bins[]{...longLiq,shortLiq}` used for **stop/target placement** | **NOT sufficient** — stops/targets anchored on fabricated levels (**P0**) | ❌ **No** — Pineify options-flow is also a proxy, not a trader-ledger cost basis. Real fix = prompt repositioning (separate decision) |
| **§8.4** | `NetFlow()` — funding×OI×mark proxy (~34K vs paid 2.56M) | none (gated off for `vergex_signal`) | N/A phase-1 | ⚠ Optional/lower priority (`get-market-tide`/`get-sector-flow-snapshot`) |
| R4 §8.1 | `confidence` = relative-max (not absolute) | rendered | P2 | ⚠ partial |
| R5 §8.3 | `handleHeatmap` parses only `symbol` (ignores `liqBand`) | — | P1 contract gap | n/a (implementation TODO, out of scope) |

**Bottom line:** Ranking is faithful and safe to depend on. Signal Lab and Heatmap reproduce schema but not semantics. Pineify directly fills **R2 (Signal Lab technicals + catalysts)** and partially **R3 (ranking quality)**, but **cannot close R1 (heatmap fabricated liquidation)** — that requires prompt repositioning as a separate decision.

---

## Design

### 1. Role and boundaries
- **Selection = Hyperliquid only.** The HL per-cohort z-score board defines the universe and the primary `Score`; Pineify never adds a symbol and never sets `Score` (keeps the `DirectionalCandidates` `|score|≥0.4` gate on the real z).
- **Pineify = AUGMENT + FILTER/BOOST.** Overlay TA/event/rating tags; an opt-in conservative qualifier may demote/remove an HL item whose **full-coverage** Pineify signal strongly contradicts the HL bias. No contradiction → no effect. Filtering is env-gated and only ever removes within the HL pool.
- **Mappable subset = US-listed `stock` classification only** (indexes/commodities/forex/pre_ipo/crypto do not map). Crypto majors = optional off-by-default later phase.

### 2. Universe → Pineify-ticker mapping
Trust `coins.go XYZCategory(base) == "stock"` (the classifier that actually feeds products); `handler_klines.go` classifiers are UI-display-only. Add a **non-US exclusion set** for `stock`-classified but non-US issues (`SMSN, SKHX, LVMH, SONY, TM, RACE, VOW3, BMW, MBG`). Build the map **dynamically each ingest** from the snapshotted xyz universe (new perps map without code changes), keyed base → bare Pineify ticker. Mappability is derived from the already-computed `Category`, not a parallel list.

### 3. Enrichment data model
Attach one nullable `Pineify *PineifySnapshot` on `asset` (`service.go:16`), filled during `Ingest()` and swapped with the snapshot under the same lock. `PineifySnapshot` holds: `Coverage` (""|"ta"|"events"|"rating"|"full"), `Bias` (composite), `Conviction` (0..1), `Trend`/`RSI`/`ADX`, `Events []PineifyEvent{Type,Name,Date}`, `Rating PineifyRating{Action,Score}`, `Error`, `FetchedAt`. **Validate each field's source in step 3** against the live tool output — it is not yet established that `get-stock-event-context` returns `{Type,Name,Date}` or `get-ai-stock-rating` returns `{Action,Score}`; the struct shape is provisional until then.

### 4. Ingest-time enrichment flow
```go
// service.go — in Ingest(), after both ingestDex calls, gated:
if s.pineifyEnabled() { s.enrichWithPineify(ctx, assets) }
```
- **Priority set ("current candidates ≈ ~10")**: the service can't see the engine's selection (decoupled), so it treats **its own top-K mappable items from the prior snapshot's ranking** as the standing board — the same board the matrix and engine consume. Enrich these every ingest.
- **HYBRID budget:** always-enrich top-K (K≈min(10,budget)) mappable items, then **rotate round-robin** through the remaining mappable universe across the leftover budget. With the default 20/min the per-3-min-interval budget is **60 calls**; 90 only applies at the 30/min ceiling. `budget` is the per-interval call cap; if `budget<10` the priority tier shrinks to `budget`.
- **Tool priority per symbol:** (1) `get-technical-analysis-snapshot` (bare ticker) + `get-stock-event-context`; (2) `get-ai-stock-rating` + optional `get-stock-research-snapshot`; (3) flow tools — **non-blocking** (unavailable/failure downgrades `Coverage`, never fails the symbol or ingest).
- Result → `PineifySnapshot`; empty/partial → degraded, product falls back to pure-HL.

### 5. Rate limiter (net-new)
New `service/signal/ratelimit.go`: token bucket, capacity+refill = `PineifyRatePerMinute`, `acquire(ctx)` respected inside the worker. Reconcile default to **20/min** (clamp `[1,30]`) — headroom under the 30/min cap for bursts/retries. Enforced at ingest layer only. Observed "temporarily unavailable" → short backoff, treat as degraded, retry next cycle.

### 6. Product integration
- **Signal Lab (`compute.go SignalLab`)** — when `Pineify != nil && Coverage != ""`, append Pineify dimension rows into the existing `dimensions[]` (respecting cap 8; free emits 3 → up to 3 Pineify rows): "Trend | Pineify Technical", "Catalyst | Upcoming Events", "Analyst | Pineify Rating"; set that row's `percentile`=conviction (fills the `-`); emit scalar `compositeZ`/`score`. **Zero formatter/client.go change.** The emitted `compositeZ`/`score` scalar is the per-symbol cohort z — but `SignalLab(symbol)` (`compute.go:352`) has no cohort context (z is computed only in `Rank()`). Step 6 must specify whether `SignalLab` **recomputes the cohort cross-section** on demand or the `asset` carries its z forward; decide in step 6.
- **Ranking/matrix (`compute.go Rank`)** — `Score`/`bias`/`rank` stay pure-HL. When `PineifyBoostEnabled=true` (default false), a mappable item with `Coverage=="full"` whose `Pineify.Bias` opposes the HL bias with `Conviction>=MinPineifyConviction` is demoted; with `PineifyHardReject=true` it is excluded. Populate the item's unused `Raw` with the `PineifySnapshot` overlay (no frontend change). Define `Coverage=="full"` as **TA + events + rating** (excludes the flow tier, which is unavailable/persistently-failing) so the qualifier is reachable.
- **Heatmap** — explicitly NOT fixed by Pineify; no "Options Flow" dimension (heatmap payload has no `dimensions[]` and `MarketAnalysis` has no extras slot to render it). Netflow — no change phase-1.

### 7. Concurrency, failure, lifecycle, cadence
All calls run inside `Start()`'s single ingest goroutine (invoked only from `Ingest()`); no new goroutines, existing snapshot `RWMutex` only. Any tool error/rate-limit → set degraded/nil, log, continue; **ingest never aborts**. Edge cases: empty top-K → skip priority tier; zero mappable stocks → no-op; token unset → wholly disabled (today's behavior).

**Cancellation must be threaded — not assumed.** Today `Ingest()` takes no ctx (`ingestCtx()` returns `context.Background()`) and `Start(ctx)` calls it with no ctx (`service.go:150,160`). To honor `Start`'s `ctx.Done()`, change `Ingest()` → `Ingest(ctx context.Context)` and pass a child ctx from both call sites in `Start()`; `acquire(ctx)` aborts on cancel.

**Wall-clock budget (protects the cadence contract).** `Config.Interval` must be ≤ the engine's min scan cadence (`config.go:6-11`). Enrichment runs synchronously inside `Ingest()` before the snapshot swap; ~60 calls × up-to-30s timeout (`service.go:40`) can delay `lastIngest`/swap and let HTTP products serve stale data past the cadence. Specify a **total enrichment wall-clock cap** (e.g. a fraction of `Interval`) and stop the budget loop when it's hit; this bounds latency rather than just guaranteeing non-abort. Accept the inherent **one-cycle enrichment lag** (enrichment targets the prior snapshot's top-K; the first ingest has none) — acceptable for a pre-entry confirmation overlay, but document it.

### 8. Config additions (additive env)
| Env | Default | Notes |
|---|---|---|
| `PINEIFY_RATE_PER_MINUTE` | **20** (clamp `[1,30]`) | now enforced by limiter |
| `PINEIFY_BOOST_ENABLED` | `false` | opt-in qualifier |
| `PINEIFY_HARD_REJECT` | `false` | only with boost; conservative |
| `PINEIFY_MIN_CONVICTION` | `0.7` | demote/reject threshold |

Existing `PineifyMCPToken`/`PineifyBaseURL` unchanged.

### 9. Reuse vs new code
Reuse: `hyperliquid.XYZCategory`/`IsXYZAsset`/`NormalizeCoinBase`; existing `dimensions[]`/`Raw` wire slots. **MCP-protocol client is NET-NEW** — `mcp/provider/` is the LLM-provider registry (claude/deepseek/openai/...) and the `mcp` package is an LLM chat client, NOT a Model Context Protocol client, so there is no reusable MCP-transport to call Pineify's `https://agents.pineify.app/mcp`. `pineify_client.go` must implement (or vendor) a real MCP client: `initialize` handshake, tool-list resolution, per-call `tools/call` JSON-RPC, and streamable-HTTP/SSE session handling if required — with its own failure/teardown surface. New: `pineify_client.go`, `pineifymap.go`, `ratelimit.go`, enrichment body in `pineify.go`. Do NOT fork `vergex` formatters or add a parallel AI-rendering path.

---

## File-by-file impact

| File | Change | Why |
|---|---|---|
| `service/signal/pineify.go` | Full implementation: `enrichWithPineify`, tool wrappers, `PineifySnapshot` builders, degraded handling, budget loop | the integration body |
| `service/signal/pineify_client.go` (new) | Thin MCP wrappers (TA bare-ticker, events, rating, research, flow) | isolated, testable against httptest fixture |
| `service/signal/pineifymap.go` (new) | `pineifyTicker(base)` — trusts `XYZCategory`, non-US + index/ETF exclusion sets | single source of mappability |
| `service/signal/ratelimit.go` (new) | Token bucket `acquire(ctx)` | net-new limiter |
| `service/signal/config.go` | default 20 + clamp `[1,30]`; add boost/hard-reject/min-conviction | enforces decisions |
| `service/signal/service.go` | `asset.Pineify` + `enrichWithPineify` in `Ingest()` | attach point + snapshot carry |
| `service/signal/compute.go` | `SignalLab()` Pineify rows/percentile/scalars; `Rank()` opt-in qualifier + `Raw` overlay | product integration; formatter untouched |
| `service/signal/http.go` | no change (optional `liqBand→binStep` = separate TODO) | wire contract stable |
| `cmd/signal-service/main.go`, web, `provider/vergex/*`, `kernel/*` | **no change** | byte-parity preserved |

**Ordering:** `ratelimit.go` + `pineifymap.go` + `config.go` (independent) → `pineify_client.go` → `pineify.go` → `service.go` → `compute.go`. All additive; no atomic landing (fully off with an unset token).

---

## Risks and migration
- **Additive, gated, reversible.** `PINEIFY_MCP_TOKEN` unset → today's output. No DB/persistence/schema/prompt change → rollback = clear token or revert-and-restart.
- **MCP protocol lifecycle (net-new).** Pineify is not a bare HTTP POST — requires `initialize` handshake, tool-list resolution, `tools/call` JSON-RPC, and (for streamable-HTTP/SSE) a session. `pineify_client.go` must handle handshake/session setup + teardown, which has its own failure surface and must be tested.
- **Sensitive-data discipline.** The token is never logged (`config.go`); also redact/mask Pineify request/response logging and do **not** persist enriched responses.
- **R1 (§8.3) not reduced by this plan** — the P0 stop/target risk is only retired by prompt repositioning (separate decision).
- **Over-trust risk** — the AI may treat Pineify ratings/TA as truth. Render Pineify rows only for `Coverage != none` symbols, keep `detail` explicit as "Pineify overlay", honor §3.7 proxy-disclosure discipline.
- **Rate-limit regression** — burst above 30/min yields "temporarily unavailable"; 20/min default + backoff guards this.
- **Classification reconciliation** — trusting `XYZCategory` promotes DXY/VIX/XLE/etc. (treated as `index` by `handler_klines`) to mappable `stock`. Add a non-mappable index/ETF exclusion set to `pineifymap.go` (avoids wasted rating calls). **Validate in step 2**: diff `XYZCategory` vs `hyperliquidXYZCategory` over the live xyz universe and harden exclusions.

---

## Implementation order
1. `ratelimit.go` + `config.go` (default 20, clamp, new flags) — compile, unit-test limiter.
2. `pineifymap.go` — mapping + exclusions; validate against live xyz universe.
3. `pineify_client.go` — wrappers; test against httptest MCP fixture (bare-ticker, rating-none-for-ETF).
4. `pineify.go` — `enrichWithPineify` (priority set, budget, tool priority, degraded handling).
5. `service.go` — `asset.Pineify` + `Ingest()` hook.
6. `compute.go` — Signal Lab rows/percentile/scalars; opt-in ranking qualifier + `Raw` overlay.
7. Tests + end-to-end smoke with a live token.

Steps 4–6 are the functional unit; step 6's qualifier is off-by-default (deployed separately).

---

## Verification (additive tests in `service/signal/pineify_test.go`)
- Mapping: `XYZCategory=="stock"` minus non-US/index exclusions → correct bare ticker; ETF → no rating call.
- Limiter: never exceeds `PineifyRatePerMinute`; 30 clamped.
- Budget: top-K always-enriched + rotation = total calls ≤ budget/interval.
- Signal Lab: enriched `dimensions[]` ≤ 8, Pineify rows present with `percentile` filled; non-mapped symbols unchanged (3 rows, `-`) — byte-parity preserved.
- Qualifier: HL-bullish + `Pineify.Bias=="bearish"` + `Conviction≥threshold` + boost/hard-reject → demoted then excluded; defaults off → overlay-only.
- Degradation: tool failure → `Coverage` downgraded, pure-HL fallback, ingest not aborted.

---

## Execution Index

| # | Goal | Done when | Key files | Depends | Size |
|---|---|---|---|---|---|
| 1 | Rate limiter + config | token bucket respects `PineifyRatePerMinute` (default 20, clamp [1,30]); new env flags parsed | `ratelimit.go`, `config.go` | none | S |
| 2 | Symbol→Pineify mapping | `pineifyTicker(base)` maps US-listed stocks, excludes non-US/index; validated against live universe | `pineifymap.go` | 1 | S |
| 3 | Pineify MCP client wrappers | thin wrappers for TA/events/rating/research/flow; tests against httptest fixture | `pineify_client.go` | 2 | M |
| 4 | Enrichment worker | `enrichWithPineify` (priority set, hybrid budget, tool priority, degraded handling) in `pineify.go` | `pineify.go` | 1,2,3 | L |
| 5 | asset attach + Ingest hook | `asset.Pineify` set in `Ingest()` under lock | `service.go` | 4 | S |
| 6 | Product integration | Signal Lab Pineify rows/percentile/scalars; opt-in ranking qualifier + Raw overlay | `compute.go` | 5 | M |

---

## Open questions
1. **Does a reusable MCP-protocol client exist, or must `pineify_client.go` implement JSON-RPC/SSE MCP from scratch (or vendor one)?** Determines step-3 size, whether the "no hand-rolled client" promise holds, and whether ordering changes (protocol client becomes a step-3 prerequisite).
2. **Live-universe exclusion hardening** — diff `XYZCategory` vs `hyperliquidXYZCategory` over the live xyz universe to finalize the index/ETF exclusion set (validate in step 2; also confirm SKHX vs SKHY).
3. **`compositeZ`/`score` in `SignalLab`** — recompute the cohort cross-section on demand vs carry the per-symbol z forward (decide in step 6).
4. **Wall-clock enrichment budget** — concrete fraction of `Interval` to cap synchronous enrichment (protects the cadence contract).
5. **`liqBand→binStep` wiring (§8 R5)** — optional, out of Pineify scope; confirm separately.
6. **Prompt repositioning of heatmap (§8.3 R1)** — separate decision-worthy change; the P0 stop/target risk persists if not actioned.

## References
- Hyperliquid info API: https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
- Pineify MCP: `https://agents.pineify.app/mcp` (bearer token = env `PINEIFY_MCP_TOKEN`, never committed)
- `docs/plans/self-built-signal-stack-2026-08-14.md` (§2.6, §3.4, §5, §8)
