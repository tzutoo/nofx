# Self-Hosted Signal Stack to Replace Claw402 Data — Plan

## Goal

Replace the paid Claw402/Vergex signal data that NOFX's strategy engine consumes (signal ranking, Signal Lab, cost/liquidation heatmap, net-flow) with a self-hosted, standalone signal service — zero claw402 data dependency — while preserving the engine's exact `vergex_signal` behavior and AI prompt format.

## Execution Index

| # | Work item | Goal | Done when | Key files | Dependencies | Size | Status |
|---|---|---|---|---|---|---|---|
| 1 | Richer Hyperliquid info structs | Service can read OI/funding/mark per symbol | `metaAndAssetCtxs` decode exposes `openInterest`/`funding`/`oraclePx` + OI-delta | `provider/hyperliquid/coins.go` | none | S | ✅ |
| 2 | Standalone signal service (core) | Four products computed + served over HTTP | `/v1/health`, `/v1/signal/ranking`, `/v1/signal/lab`, `/v1/signal/heatmap`, `/v1/netflow/ranking` all respond from a live ingest snapshot | `service/signal/*`, `cmd/signal-service/main.go`, `Dockerfile.signal`, `docker-compose.signal.yml` | 1 | XL | ✅ |
| 3 | `vergex.Client` transport rewrite | Engine talks to service, not claw402 | `NewClient(baseURL, logger)`; plain GET `doGET`; `client_test.go` still green; retryable-error surface | `provider/vergex/client.go`, `client_test.go` | 2 (must land atomically — see below) | M | ✅ |
| 4 | Engine repoint | No claw402 routing in data path | `NewStrategyEngine` drops `SetClaw402`, always builds vergex client at `SIGNAL_SERVICE_BASE_URL`; `auto_trader.go` unchanged, `api/strategy.go` edited to relax `resolveStrategyDataWalletKey` | `kernel/engine.go`, `api/strategy.go` | 3 | S | ✅ |
| 5 | Prompt disclosure line | AI told heatmap/net-flow are proxies | disclosure line added once to `vergexHoldRules()` (single English function); `engine_prompt_test.go` still passes; new test asserts disclosure substring | `kernel/engine_prompt.go` | none | XS | ✅ |
| 6 | Degradation hardening | Service-down doesn't abort cycle | `Run` loop + `api/strategy.go` tolerate `GetCandidateCoins` error; verify `Run` first (prerequisite) | `trader/auto_trader.go`, `api/strategy.go` | 4 | S | ✅ |
| 7 | Pineify integration (optional) | Pineify augments `xyz:` TradeFi signal + technicals for ~20 crypto majors; long-tail HL stays pure-HL | gated behind `PINEIFY_MCP_TOKEN`; rate-limited to `PINEIFY_RATE_PER_MINUTE` inside the ingest worker only; never blocks 1–6 | `service/signal/pineify.go` | 2 | M | 🚫 deferred |

Size legend: XS < S < M < L < XL.

> **Status legend:** ✅ implemented + tested · 🚫 intentionally not built
>
> **Item 7 (Pineify) — deferred, not a gap.** The core mission (removing the paid claw402 dependency) is fully met by items 1–6 without Pineify. Pineify would reintroduce an *external* provider (paid, behind `PINEIFY_MCP_TOKEN`) to augment `xyz:` TradeFi technicals for ~20 crypto majors. Building it is optional and does not advance the claw402-removal goal. Revisit only if TradeFi technical augmentation is explicitly wanted.
>
> **Backfill — not implementable as a task.** (1) *Population* cost-basis/heatmap backfill is **impossible**: `clearinghouseState` is per-wallet **current-only** in public Hyperliquid data, so historical entry/liquidation levels for the population cannot be reconstructed (see §3.3.2, §5). (2) *Self-wallet* shallow history via a rolling position log is **buildable but forward-only**: it accumulates from when logging starts and cannot retroactively backfill. Both are documented hard limits, not skipped work.

---

## 1. Summary

Replace the paid Claw402/Vergex data feed (rank, Signal Lab, heatmap, net-flow) with a **self-hosted standalone Go signal service** that ingests free Hyperliquid info data (optionally augmented by the Pineify MCP server) and serves all four products over HTTP. The engine keeps its exact `vergex_signal` behavior by **repointing the existing `vergex.Client` at the new service and replacing its x402 transport with a plain HTTP GET** — leaving `SignalRankingData` / `SignalRankItem` / `MarketAnalysis` shapes, `Context.VergexDataMap`, `vergexRankingCache`, `DirectionalCandidates`, and the AI prompt pipeline untouched, so prompt output is byte-equivalent by construction. Computation lives in the standalone service (its own binary/container), and the engine's nofxos net-flow path is de-flagged from claw402 routing. Two honest gaps are flagged and designed around as documented proxies: (a) Claw402's cost/liquidation heatmap relies on a full-book trader ledger that Hyperliquid public `info` does not expose, and (b) institution/personal net-flow is not publicly derivable. The plan delivers structural (byte) parity on all four products with clearly-labelled proxy semantics and an additive one-line prompt disclosure so the AI does not over-trust proxy data.

**User decisions (fixed):** (1) full parity on all four products; (2) standalone container (separate service, not in-process); (3) Pineify MCP as external analytics source — **tested: full US equities/options toolset + technical-analysis for ~20 Binance crypto majors** (see §2.6); (4) fully drop claw402 data.

---

## 2. Current-state analysis

### 2.1 The paid seam — `provider/vergex/client.go`

`vergex.Client` owns four data methods, all routed through `doGET` → `payment.DoX402Request` (claw402 x402 gateway, `DefaultBaseURL = "https://claw402.ai"`):

- `GetSignalRanking(ctx, Query) (*SignalRankingData, error)` → `/api/v1/vergex/signal-ranking`
- `GetSignalLab(ctx, Query) (json.RawMessage, error)` → `/api/v1/vergex/signal-lab`
- `GetCostLiquidationHeatmap(ctx, Query) (json.RawMessage, error)` → `/api/v1/vergex/cost-liquidation-heatmap`
- `GetFlowMarkets(ctx, chain, window, limit) (json.RawMessage, error)` → `/api/v1/vergex/flow-markets` — **never called by the engine**; it exists only as an API-client method.

The x402 signing (`MakeClaw402SignFunc`, `SignBasePaymentHeader`, `DoX402Request`) is in `mcp/payment/x402.go` and is the payment path to bypass. `NewClient(baseURL, privateKeyHex, logger)` requires a wallet key (env `CLAW402_WALLET_KEY`) and fails without one.

Model shapes (the byte-parity contract):
- `SignalRankingData{Raw json.RawMessage, Items []SignalRankItem}`
- `SignalRankItem{Rank int, Symbol, MarketType, Bias string, Confidence, Score float64, Category string, Raw json.RawMessage}`
- `MarketAnalysis{Symbol, QuerySymbol, MarketType string, Ranking *SignalRankItem, SignalLab, Heatmap json.RawMessage, SignalLabError, HeatmapError string}`

Parsing is defensive and field-order/case-insensitive (`parseRankItem`, `findObjectArray`, `normalizeKey`, `decodeVergexDataObject`). AI rendering is `FormatAnalysisForAI` → `FormatSignalLabMarkdown` (reads `dimensions[]` with `family/label/direction/strength/percentile/detail`) and `FormatHeatmapMarkdown` (reads `bins[]` with `bucketStartPrice/bucketEndPrice/px/longCost/shortCost/longLiq/shortLiq`, `binStep`). **These field names are the wire contract the new service must emit.**

Symbol/XYZ tiering (`isTradeFi/isAll/isCoreMarketType`, `TradableSymbolForMarket`, `MarketSymbol`, `QuerySymbol`, `QueryChain`) and `hyperliquid.IsXYZAsset` encode the `hip3_perp` → `xyz:` prefix semantics; any source must mirror this or ranking/detail filtering mis-tags.

### 2.2 Engine consumption path — `kernel/engine.go`

- `NewStrategyEngine(config, claw402WalletKey...)` (L197): builds `nofxosClient`; when a wallet key exists it (a) routes nofxos through `Claw402DataClient` via `client.SetClaw402`, and (b) builds `vergexClient`. Otherwise `vergexClient` stays nil → `vergex_signal` is effectively disabled.
- `GetCandidateCoins` (L414) dispatches `case "vergex_signal"` → `getVergexSignalCoins` (L730): calls `GetSignalRanking`, then `FilterSignalRankingItems`, builds `vergexRankingCache map[string]*vergex.SignalRankItem`, and does **direction-balanced selection** (interleave bull/bear by `Bias`) into `[]CandidateCoin`.
- `DirectionalCandidates` (L892): reads `vergexRankingCache`, sorts bull/bear by `Rank` asc, returns `[]DirectionalCandidate{Symbol, Score}` — consumed by `auto_trader_force.go` `ensureLongShortCoverage` (needs `|score| >= 0.4`, `vergex_signal` only).
- `FetchVergexDataBatch` (L1122): per-symbol parallel `GetSignalLab` + `GetCostLiquidationHeatmap` (concurrency 2, timeout 45s), with `vergexDetailQueryCandidates` fallback + `isRetryableVergexDetailError`; feeds `map[string]*vergex.MarketAnalysis`.
- `enrichVergexDataWithStrategy` (`engine_analysis.go:146`): fills `Context.VergexDataMap` for `vergex_signal` only → `formatVergexData` → `vergex.FormatAnalysisForAI` into the prompt.
- `FetchNetFlowRankingData` / `FetchOIRankingData` / `FetchPriceRankingData` (L1399-1473): route to **nofxos** (not vergex), gated by `EnableXRanking` flags and **skipped** when `usesHyperliquidNativeUniverse()` (engine.go:247).
- `api/strategy.go` (L520-650) is a second concrete consumer (`FetchVergexDataBatch`) — the parity harness.

### 2.3 Config — `store/strategy.go`

`CoinSourceConfig` (L770) has `SourceType` + `VergexLimit/MarketType/Chain/LiqBand`. `GetDefaultStrategyConfig` defaults `SourceType: "vergex_signal"`. `ClampLimits` clamps `VergexLimit`. No schema migration is needed if we keep these fields as the selection + query surface. Wiring happens in `trader/auto_trader.go` `reloadStrategyConfigIfChanged` (L395) → `NewStrategyEngine(config, claw402Key)`.

### 2.4 Free raw sources already in-repo

- `provider/hyperliquid/coins.go`: `fetchPerpDexCoins` POSTs `{"type":"metaAndAssetCtxs"}` to `api.hyperliquid.xyz/info` (private `assetCtx` decodes only `DayNtlVlm/MarkPx/PrevDayPx`).
- `provider/hyperliquid/kline.go`: info client `GetAllMids/GetAllMidsXYZ/GetMeta/GetCandles`.
- `api/handler_hyperliquid_wallet.go:81` `hyperliquidClearinghouseState` + `cmd/e2e_builder_fee/main.go`: exact `clearinghouseState` JSON shape (`marginSummary/crossMarginSummary/withdrawable/assetPositions[].position.{szi,entryPx,positionValue}`) for per-wallet cost-basis / book-PnL reconstruction.

### 2.5 Reference ranking products — `provider/nofxos/`

`netflow.go` defines `NetFlowRankingData{Duration,TimeRange,InstitutionFutureTop/Low,PersonalFutureTop/Low}` of `NetFlowPosition{Rank,Symbol,Amount,Price}` + `FormatNetFlowRankingForAI`. `oi.go`/`price.go` are sibling shapes/formats. These are the canonical net-flow shape/format the signal service should follow.

### 2.6 Pineify MCP probe — tested coverage (from live tool calls)

Pineify covers **US equities/options broadly, plus technical analysis for ~20 Binance crypto majors.** Tested split:

| Coverage | Tools | Symbols |
|---|---|---|
| **US equities/options** (full) | `find-ai-stock-picks`, `get-ai-stock-rating`, `screen-stocks`, `detect-market-regime`, `research-stock`, `get-stock-research-snapshot`, `get-technical-analysis-snapshot`, `get-stock-event-context`, `find-dark-pool-trades`, `find-options-flow-alerts`, `get-market-tide`, `get-sector-flow-snapshot`, `track-smart-money` | US-listed (S&P500/NDX/DOW + broader) |
| **Crypto (Binance)** — **technical-analysis only** | `get-technical-analysis-snapshot` (300 fields/timeframe, e.g. BINANCE:BTCUSDT/ETHUSDT) | ~20 USDT majors: BTC, ETH, BNB, XRP, SOL, TRX, DOGE, WBTC, ZEC, ADA, XLM, WLFI, ASTER, LINK, BCH, GRAM, WBETH, LTC, HBAR, AVAX |

**No crypto ratings/research/flow:** `get-ai-stock-rating`, `research-stock`, and `get-stock-research-snapshot` return `symbol_not_found` for crypto (US stocks only). So Pineify's crypto value is **technical signal augmentation for the top ~20 majors**, not a full-universe or ratings source.

**Tool → signal-product mapping (US equities / options; + crypto technicals):**

| Signal product | Pineify tools |
|---|---|
| Signal ranking (bias/z-score) | `find-ai-stock-picks`, `get-ai-stock-rating`, `screen-stocks`, `find-technical-setups` (scan universe by preset), `detect-market-regime` (US stocks); crypto has no rating tool |
| Signal Lab (fundamentals/technicals/levels) | `research-stock`, `get-stock-research-snapshot`, `get-technical-analysis-snapshot` (300-field), `get-stock-event-context`, `analyze-earnings`; crypto → `get-technical-analysis-snapshot` only |
| Cost/liquidation | partial: `find-dark-pool-trades`, `find-options-flow-alerts`, `get-option-contract-flow` (options-flow proxies, not true cost-basis) |
| Net-flow / sentiment | `get-market-tide`, `get-sector-flow-snapshot`, `find-options-flow-alerts`, `track-smart-money`, `find-congress-trades`, `analyze-sector-rotation` |

**Out of scope for this signal stack (part of the 28, not used):** the five code-validation tools (`pine-script-syntax-checker`, `mql5-syntax-checker`, `mql4-syntax-checker`, `ctrader-csharp-syntax-checker`, `ninjascript-syntax-checker`) and the portfolio/workflow tools (`analyze-portfolio-risk`, `optimize-portfolio`, `suggest-options-strategy`, `validate-trading-idea`) — none feed the four signal products. Note: the catalog describes every tool as "US", but `get-technical-analysis-snapshot` demonstrably resolves Binance crypto symbols (verified live), so crypto support is real but undocumented.

**Live verification (all needed tools exercised):** 16/19 confirmed working — ranking (`find-ai-stock-picks`, `get-ai-stock-rating`, `screen-stocks`, `find-technical-setups`, `detect-market-regime`), Signal Lab (`research-stock`, `get-stock-research-snapshot`, `get-technical-analysis-snapshot`, `get-stock-event-context`, `analyze-earnings`), cost/liq (`find-dark-pool-trades`, `find-options-flow-alerts`), net-flow (`get-market-tide`, `get-sector-flow-snapshot`, `analyze-sector-rotation`, `find-congress-trades`). **`generate-market-briefing` and `track-smart-money` are persistently unavailable** (confirmed on retry — service-side outage, not parameter errors); design must treat them as optional/non-blocking. **Workflow note — `get-option-contract-flow`:** it requires an exact contract ID, which the `find-options-flow-alerts` **summary** view hides; the actual structured alert objects returned by `find-options-flow-alerts` do contain `contractId`, so the ingest worker parses it from the full objects before calling `get-option-contract-flow` — not a real workflow blocker.

**Multi-tool composition (verified):** Pineify tools compose into a rich per-symbol briefing — e.g. `get-technical-analysis-snapshot(NVDA)` + `get-technical-analysis-snapshot(BTCUSDT)` + `detect-market-regime` + `analyze-sector-rotation` yields per-symbol trend/RSI/momentum/EMA/ADX context plus regime + sector context in one pass (validated on both a US stock and a Binance crypto symbol). The ingest worker should follow this **compose-multiple-tools-per-refresh** pattern to build Signal-Lab/ranking enrichment, subject to the `PINEIFY_RATE_PER_MINUTE` budget (batch per refresh, never per-symbol per-request).

**Relevance to NOFX's universe:** the built-in strategy's signal board is dominated by `xyz:` TradeFi perps (NVDA, SNDK, MU, SP500 — US-equity synthetic markets), which Pineify's US-equity tools map to directly. **Crypto majors (BTC/ETH/SOL, etc.) can get Pineify technical augmentation** (mapping `BTCUSDT`→HL `BTC` core_perp), but the **long-tail HL universe and the HL-native signal computation (funding/OI/heatmap) stay 100% self-built.** Pineify is never a hard dependency; rate limit is real (observed `temporarily unavailable` + bounded/partial responses) — enforce at the ingest layer only.

---

## 3. Design

### 3.1 Decision: which side holds the parsing — and how the engine switches

**Decision: repoint `vergex.Client` at the self-hosted service; replace the x402 transport with a plain HTTP GET; keep all parsing/model/format logic in the `vergex` package unchanged.** Do not introduce a parallel provider behind an interface.

Rationale: byte-equivalent prompts and zero behavioral drift are guaranteed because the engine's models (`VergexDataMap`, `vergexRankingCache`, `DirectionalCandidates`) and the AI formatters (`FormatAnalysisForAI`) are literally the same functions, now consuming data from the new service. A "new provider behind the same interface" would force a shared-model abstraction that touches `Context.VergexDataMap`'s type, `StrategyEngine.vergexClient`'s type, and every consumer — high churn for a cosmetic rename. The downside (the `vergex` package name embedding a former vendor) is acceptable and can be renamed in a later non-behavioral cleanup.

**Changes to `vergex.Client`:**
- Remove the `privateKey` field and the x402 path. `NewClient` no longer requires/derives `CLAW402_WALLET_KEY`; it takes `(baseURL string, logger mcp.Logger)` and never fails on missing credentials (constructor becomes infallible for security/missing-key). If `baseURL == ""` it falls back to env `SIGNAL_SERVICE_BASE_URL`, else `http://localhost:8480`.
- `doGET` becomes a plain GET against `c.baseURL + path + query` with `X-Client-ID: nofx`; no `payment.DoX402Request`, no `MakeClaw402SignFunc`. Non-200 responses are wrapped so their message contains the retryable markers the engine already checks: `"invalid markettype"`, `"invalid chain"`, `"market not found"`, `"not_found"` for 4xx, and 5xx/503 for stale service.
- Signatures of the four methods and `ParseSignalRanking` are unchanged.
- `GetFlowMarkets(ctx, chain, window, limit)` is retargeted at the service endpoint (still returns `json.RawMessage`).

**Change to `nofxos` claw402 routing:** in `NewStrategyEngine`, delete `client.SetClaw402(newClaw402DataClient…)`. `nofxos.Client.doRequest` then goes direct to `nofxos.ai`. **Moot in this plan's scope (review-verified):** for `vergex_signal`, `usesHyperliquidNativeUniverse()` (engine.go:247) is true, which makes `FetchOIRankingData`/`FetchNetFlowRankingData`/`FetchPriceRankingData` all return nil — so `nofxosClient` is never invoked in the vergex_signal path; the shared `DefaultAuthKey` risk does not fire here. Note also `NewStrategyEngine` prefers `config.Indicators.NofxOSAPIKey` before `DefaultAuthKey` (engine.go:200-203). `mcp/payment/x402.go`, `provider/nofxos/claw402.go`, and the dead `provider/nofxos/client_resolve.go` (`ResolveClient`, zero in-repo callers) are **retained but not data-plane dependencies** — the AI LLM Claw402 client `mcp/payment/claw402.go` and the payment library stay for AI chat; the rest is deferred cleanup.

### 3.2 The standalone service — location, structure, lifecycle

New module-internal packages (same Go module, separate binary — so both `vergex` models and `hyperliquid` helpers are shared imports, guaranteeing shape symmetry):

- `cmd/signal-service/main.go` — entrypoint: load env, construct `service/signal.Service`, start `http.Server`, `signal.NotifyContext` graceful shutdown.
- `service/signal/service.go` — the `Service` coordinator: owns config, ingest loop, computed snapshot, and product accessors.
- `service/signal/config.go` — `Config` struct parsed from env (see §3.4).
- `service/signal/ingest.go` — HL polling worker.
- `service/signal/compute.go` — the four product computations.
- `service/signal/http.go` — HTTP handlers + routing.
- `service/signal/pineify.go` — optional Pineify MCP client (gated by token presence).

**Execution model:** a single ingest goroutine polls Hyperliquid on `Config.Interval` (default `3m`, must be `<=` engine min scan cadence 3m so the cache is never older than a scan). It writes an immutable snapshot under `sync.RWMutex`. HTTP handlers read the snapshot and compute products on demand from it (products are cheap cross-sectional reductions of the snapshot, not network calls), then cache each product with an in-memory time-based TTL equal to the interval. Concurrent reads are safe; a product is served stale (or 503 when past the staleness threshold, see §3.6). No persistence is required in phase 1; persistence/backfill is a separate concern (§5).

**External rate limiting (Pineify) — enforced at the ingest layer.** The Pineify MCP is rate-limited to only a few calls per minute, so the service must never call Pineify on the request path or in the trading loop. All Pineify calls are batched inside the single ingest worker at a throttled cadence (`Config.PineifyRatePerMinute`, default low, e.g. 3), with results written into the same immutable snapshot that HTTP handlers serve. If the ingest loop has not refreshed the Pineify-derived fields within the rate window, those fields are served stale (or omitted/degraded per §3.6), never re-fetched on demand. This keeps external-call volume flat and bounded regardless of how often the engine polls or how many symbols the strategy selects.

**Universe:** crypto perps (dex `""`) always; `xyz:` TradeFi assets (dex `"xyz"`) when `category/marketType` asks for them (mirroring `vergex` tiering). The engine's existing `category` filter (`HyperRankCategory` → "all"/"stock"/etc.) is applied client-side, unchanged, in `getVergexSignalCoins`; the service serves the full ranked board and the engine filters, so no source-tier logic is forked.

### 3.3 Signal products — computation, inputs, outputs

#### 3.3.1 Signal ranking (bias + z-score) — genuinely computable from public HL data

Input per symbol (from the snapshot): current `markPx`, `prevDayPx`, current funding, current `openInterest`, and OI-delta computed as `current OI − prior poll OI` (the service keeps the previous snapshot; HL's `metaAndAssetCtxs` exposes `openInterest`/`funding` per asset — the richer fields the current `assetCtx` ignores, see §4 `provider/hyperliquid/coins.go`). **Decision (per review): no candle dependency in v1.** `metaAndAssetCtxs` already provides `prevDayPx` (24h delta) for free; do NOT fetch per-symbol candles for 1h/4h deltas or ATR in the ranking — drop those factors to keep the 3m ingest worker cheap and avoid Hyperliquid 429 rate-limit risk. Candle-based ATR is deferred to a later enhancement for the few detail symbols only. **Cold-start rule:** on the first interval there is no prior snapshot, so OI-delta is undefined; compute the ranking only from factors with valid prior data (omit OI-delta and netflow-delta for the first interval), applying the same "missing factor → omit symbol" rule — never emit OI-delta=0 as if it were real.

Computation — **per-cohort** cross-sectional z-scores (decision per review): compute z-scores **separately within each market cohort** (`core_perp` crypto as one cohort, `hip3_perp`/xyz TradeFi as another) so high-vol crypto does not mute low-vol TradeFi in a single cross-section, then merge both cohorts into one ranked board. This keeps the exact `Score` values `DirectionalCandidates`/`auto_trader_force` act on meaningful:
```
z_x = (x_sym - mean_x) / std_x      // per factor, per cohort, across the universe
composite = w1*z_price24h + w2*z_funding + w3*z_oiDelta
w = [0.45, 0.35, 0.20]              // normalized weights (netflow excluded from ranking in v1)
score      = composite              // board z-score; negative → bearish (DirectionalCandidates depends on sign)
confidence = clamp(|composite| / maxAbsComposite, 0, 1)
rank       = 1-based index by |composite| descending (strongest first)
bias       = "bullish"  when score > 0
           | "bearish"  when score < 0
           | "neutral"  when score == 0
```
**Bias token contract (decision per review):** emit the exact literal strings the engine branches on — `"bullish"`, `"bearish"`, `"neutral"` (and the accepted aliases `"long"/"short"/"buy"/"sell"`). Never emit a sign or `+/-`. `getVergexSignalCoins` (engine.go:873) and `DirectionalCandidates` (engine.go:947) branch on these exact tokens; any other value drops the item into the `"other"` bucket and breaks direction balancing.

Emits `SignalRankingData{Items: []SignalRankItem}` where each item carries `Rank/Symbol(querySymbol)/MarketType/Bias/Confidence/Score/Category` (+ `Raw`). `MarketType` must be emitted as `hip3_perp` for xyz assets and `core_perp` for crypto so `FilterSignalRankingItems`/`isRetryableVergexDetailError`/detail-market-type inference behave as today. `Category = hyperliquid.XYZCategory(base)` for xyz, `"crypto"` for core. The service must return symbols that survive `vergex.QuerySymbol` (strip `xyz:`/`USDT`) so `FetchVergexDataBatch`'s `MarketAnalysis.Symbol/QuerySymbol` normalization and the `FormatAnalysisForAI` header `### %s (Vergex %s/%s)` stay correct.

Edge cases: universe std=0 → treat all z as 0; missing funding/OI for a symbol → omit that symbol from the product (do not emit zero-score noise), which the engine already tolerates; empty universe → return empty `Items` (engine's existing "no tradable items" error path handles it). **Ranking must yield BOTH bullish and bearish items** when the cohort is directionally split (a symmetric universe must not collapse to one side), since `getVergexSignalCoins` interleaves long/short candidates.

#### 3.3.2 Cost / liquidation heatmap — honestly a *proxy*

**Flagged gap:** Claw402's heatmap aggregates many traders' `entryPx`/`liquidationPx`. Hyperliquid public `info` exposes `clearinghouseState` **per-wallet only** and has no global position ledger. A true trader-cost heatmap is not derivable from public data. The plan ships a structurally-parity proxy and discloses it (§3.7), so the AI never mistakes proxy for truth.

Computation — build a price histogram and emit the exact `bins` schema (`bucketStartPrice`, `bucketEndPrice`, `px`, `longCost`, `shortCost`, `longLiq`, `shortLiq`) + `binStep`:
- **binStep via volatility proxy (decision per review — no candle/ATR dependency in v1):** `binStep = max(0.1%, min(2%, 2×|prevDayPx − mark| / mark)) × mark`, i.e. derived from the free `prevDayPx`/`mark` move rather than ATR14. Candle-based ATR is a later enhancement.
- Range = `[mark - 6×binStep-ish, mark + 6×binStep-ish]`, bucketized by `binStep`.
- **USD scaling (decision per review):** convert HL `openInterest` (base-coin units) to USD notional `OI_usd = OI × mark`, and use that as the total cost/liquidation pool distributed across bins by the pressure proxy. This makes `longCost/longLiq/shortCost/shortLiq` naturally **USD-scale**, so `FormatHeatmapMarkdown` renders `$K/$M` correctly and matches the pinned `"$1.20M"` test.
- **Weighted OI pressure proxy** (the only defensible population-level signal): signed exposure `E = OI_usd × sign(funding)` (decision per review: the undefined `maxLeverage_implied = OI/estimatedMargin` is dropped — no `estimatedMargin` variable); flow is redistributed across bins with a falloff `~ 1/(|mark-bin|/binStep)^2`, clipped at `±6×binStep`. `longCost/longLiq` receive the positive-exposure weight, `shortCost/shortLiq` the negative-exposure weight (split by `sign(funding)`).
- **Self-wallet real data** (when `HYPERLIQUID_WALLET_ADDR` is set): for each open position, place a real spike into the bucket containing `liquidationPx` (longLiq if long, shortLiq if short); place `positionValue` into the `entryPx` bucket as `longCost`/`shortCost`. **Required struct addition (decision per review):** no in-repo struct currently decodes `liquidationPx` — `hyperliquidClearinghouseState` (api/handler_hyperliquid_wallet.go:81) decodes only `szi/unrealizedPnl`, and the e2e struct (cmd/e2e_builder_fee/main.go:22) decodes `entryPx/positionValue` but not `liquidationPx`. The service must add `liquidationPx` (and `entryPx`) to a `clearinghouseState`-shaped struct. This is an explicit, flagged field addition, not implied.
- Totals and `MainCluster` are derived by the existing `FormatHeatmapMarkdown` unchanged.

#### 3.3.3 Signal Lab (POC / levels / book PnL) — proxy on structure + real book PnL

Emits the `dimensions[]` schema (`family/label/direction/strength/percentile/detail`) + `symbol/marketType/band/bias/confidence/compositeZ/score`. Rows (top 8, matching `FormatSignalLabMarkdown`'s cap):

| family | label | derivation | direction | strength | percentile |
|---|---|---|---|---|---|
| Market Structure | Point of Control (POC) | bin with max weight from §3.3.2 histogram | above/below mark | high/med/low from bin weight | cross-sectional bin percentile |
| Liquidity | Nearest liquidation cluster | min-distance bin with max `liq` weight | direction of that cluster | from cluster mass | — |
| Book PnL | Open-position book PnL | `Σ szi×(mark−entryPx)` from self `clearinghouseState`; strength by `|PnL|/equity` | sign of PnL | — |
| Trend | Momentum | sign/magnitude of 4h/24h delta | sign | from normalized delta | percentile |
| Funding | Funding pressure | funding × OI | sign of funding | | |

`compositeZ`/`score` reuse §3.3.1's per-symbol composite. `percentile` = cross-sectional percentile of that row's metric. When `HYPERLIQUID_WALLET_ADDR` is unset, Book PnL row is omitted (dimension skips, never fabricated).

#### 3.3.4 Net-flow — structural parity with a documented proxy

**Flagged gap:** a true institution/personal split is not computable from HL public data. Deliver **structural parity** matching `nofxos.NetFlowRankingData`/`NetFlowPosition{Rank,Symbol,Amount,Price}` and the same in-prompt tables, with defensible proxies:
- `InstitutionFutureTop/Low` (smart-money proxy): `Amount = funding_rate × OI × mark` over the window (capital accruing to the funding<0 side — who the leveraged consensus is leaning against). Rank by `|Amount|` desc.
- `PersonalFutureTop/Low` (retail proxy): `Amount = OI_delta_current_window × mark` (new/closed margin in the square the rank calls "retail" = deviation from funding-implied consensus). Rank by `|Amount|` desc.
- `Price` = current mark; `Rank` = 1-based by `|Amount|` desc.

The `vergex_signal`/HL-native strategies previously *skipped* nofxos netflow (`usesHyperliquidNativeUniverse`). The service net-flow therefore **fills a previously-empty feed** rather than replacing a live one. **Confirmed decision (per review):** phase-1 keeps `FetchNetFlowRankingData` on the free `nofxos` provider (unchanged); the service's `/v1/netflow/ranking` endpoint is shipped for API-completeness and future use but is **NOT consumed by the engine in phase-1**. This satisfies the goal — net-flow is already not a claw402 cost in the current engine path — and routes the engine's net-flow to the service is deferred to phase-2. Window semantics: define `window` = 1h (HL funding is hourly) and `OI_delta_current_window` = OI delta over one ingest interval scaled to the window.

#### 3.3.5 Caveat vs. Claw402 — proxy, not order-flow

**What NOFX always consumed from Claw402 is the same derived surface** (institution/personal net-flow amounts, OI deltas, funding, POC/heatmap levels). NOFX **never received raw order-flow** (trade/execution tape, order-book depth, aggressor-side prints) from Claw402 — those fields above are the full extent of the feed it exposed. So this plan does **not** regress NOFX's order-flow access; it reproduces the identical output contract.

**The unknown is Claw402's *internal* derivation.** Whether Claw402 computed its institution net-flow / heatmap from real order-flow tape or from funding/OI proxies is proprietary and not visible from the delivered payload. Two cases:

- If Claw402 derived from funding/OI the same way, the self-hosted values are functionally equivalent.
- If Claw402 used real trade/order-flow tape internally, the self-hosted `Amount`/`bins` magnitudes are **proxies of the same shape, not Claw402-equivalent numbers** — the AI should treat them as directional/proxy signals, not as Claw402 parity. The §3.7 disclosure line already tells the AI this.

**Implication:** match on *shape and format* (byte-parity), **never** on absolute numeric parity with past Claw402 values. Do not use Claw402-derived numbers as a regression benchmark for the service's output.

### 3.4 Config and source selection

All new settings are env vars (no DB schema change, no `store/strategy.go` migration; `SourceType: "vergex_signal"` remains the selection, `VergexLimit/MarketType/Chain/LiqBand` remain the query surface passed to the service):

| Env | Consumer | Default | Notes |
|---|---|---|---|
| `SIGNAL_SERVICE_BASE_URL` | engine `vergex.NewClient` | `http://localhost:8480` | no trailing slash |
| `SIGNAL_SERVICE_LISTEN` | service | `:8480` | |
| `SIGNAL_SERVICE_INTERVAL` | service ingest | `3m` | ≤ engine min scan (3m) |
| `HYPERLIQUID_WALLET_ADDR` | service | unset | enables real book PnL + real liq anchors |
| `PINEIFY_MCP_TOKEN` | service (secret) | unset | **never committed**; gated |
| `PINEIFY_BASE_URL` | service | `https://agents.pineify.app/mcp` | |
| `PINEIFY_RATE_PER_MINUTE` | service ingest | `3` | bounds Pineify calls/min; enforced only in the ingest worker, never on the request path |

`NewStrategyEngine(config, claw402WalletKey...)` signature is unchanged but its body ignores `claw402WalletKey` for routing (nofxos direct, vergex at `SIGNAL_SERVICE_BASE_URL`). This keeps `auto_trader.go` and `api/strategy.go` untouched. Stretch (out of phase-1): optional `CoinSource.SignalServiceBaseURL` per-strategy override — noted only, not specced, to keep `ClampLimits`/defaults stable.

### 3.5 HTTP API of the service

Plain GET, JSON. Endpoints designed so `vergex.Client` paths + query params map 1:1:

| Endpoint | Engine client method → path/params | Response shape |
|---|---|---|
| `GET /v1/signal/ranking?chain=&liqBand=` | `GetSignalRanking` → `SignalRankingPath` (`/api/v1/vergex/signal-ranking`) — note `addQueryDefaults(..., false)` sends only `chain` + `liqBand` (no `marketType`, no `limit`; filtering is client-side) | `SignalRankingData` |
| `GET /v1/signal/lab?symbol=&marketType=&chain=&liqBand=` | `GetSignalLab` → `SignalLabPath` | `SignalLab` raw (dimensions) |
| `GET /v1/signal/heatmap?symbol=&marketType=&chain=&liqBand=` | `GetCostLiquidationHeatmap` → `CostLiquidationHeatmapPath` | Heatmap raw (bins) |
| `GET /v1/netflow/ranking?chain=&window=&limit=` | `GetFlowMarkets` | `json.RawMessage` (nofxos `NetFlowRankingData` shape) |
| `GET /v1/health` | ops | `{"ok":true}` + last-ingest age |
| `GET /v1/products` | ops | capabilities + staleness per product |

The service must interpret `chain` (`hyperliquid`/`mainnet` → HL perps; empty → default) and `symbol` (strip `xyz:`/`USDT` via the shared `vergex.QuerySymbol`; `marketType` variants normalized via shared `MarketSymbol`). **`liqBand→binStep` mapping (fixed, per review):** `liqBand` is an opaque passthrough (config default `""`; UI sends `"15"`). Freeze the mapping — `""`→1×, `"5"`→0.5×, `"10"`→0.75×, `"15"`→1×, `"20"`→1.5× of the default volatility-proxy `binStep` from §3.3.2 — and document it so the service interprets the param deterministically. All endpoints return 404 for an unknown symbol (message contains `"market not found"` → engine's `isRetryableVergexDetailError` triggers its fallback-candidate logic).

### 3.6 Graceful degradation / error surface

- **Service down / 5xx / 503 (stale ingest):** `GetSignalRanking` errors → `getVergexSignalCoins` returns error → `GetCandidateCoins` errors — the engine's existing behavior must be left to **not abort the cycle**. Verify `auto_trader.go`'s `Run` loop already continues on `GetCandidateCoins` error (manage existing positions only); add a tolerant branch if it propagates. Detail endpoints (`lab`/`heatmap`): current per-symbol `SignalLabError`/`HeatmapError` strings already render `"unavailable (...)"` in the prompt, and `enrichVergexDataWithStrategy` scopes failures to the symbol — no change.
- **Stale-serving policy:** if `now − lastIngest > Interval×2`, serve stale with an `X-Signal-Stale: true` header and set product `stale` flag; at `Interval×4` return 503 (retryable 5xx). Empty-but-healthy universe → 200 with empty `Items` (never 503), letting the engine's "no tradable items" path handle it.
- **Pineify down:** the Pineify-augmented products fall back to pure-HL computation and set the product `degraded` flag; ranked/lab/heatmap/netflow remain fully functional without Pineify. Pineify is never a hard dependency.

### 3.7 Prompt disclosure (the one additive text change)

Because heatmap/book/net-flow are labelled proxies, add one additive disclosure line to `vergexHoldRules()` (`kernel/engine_prompt.go`). **Correction from critique:** `vergexHoldRules()` is a single, argument-less function returning fixed **English** text, written into `buildVergexSystemPrompt` via `sb.WriteString(vergexHoldRules())` — there is no separate zh variant. So add the line once, in plain English (required, since `engine_prompt_test.go` enforces English-only via `containsCJK`). Add a test asserting the disclosure substring appears in the rendered prompt. Example fragment: `"- Data note: cost/liquidation heatmap and net-flow are self-built proxies derived from public funding/Open-Interest/mark data, not a full trader-book ledger; treat as stress indicators, not absolute liquidation levels."`

This is safe against `engine_prompt_test.go` because its assertions use `strings.Contains` (checks presence, not exact equality) — adding a line preserves them. It is the single intentional prompt-text change; required substrings (`Claw402.ai Signal Ranking` etc.) remain.

### 3.8 Concurrency, lifecycle, cancellation

- Service: single ingest goroutine; snapshot swap under `sync.RWMutex`; HTTP handlers never mutate the snapshot. No cross-goroutine sharing of product caches. Graceful shutdown drains in-flight requests then stops the ingest ticker.
- Engine: `FetchVergexDataBatch` already parallelizes with `vergexDetailSymbolConcurrency=2` and `vergexDetailRequestTimeout=45s`; unchanged. New `vergex.doGET` must honor the request `ctx` (cancel propagates to the HTTP client) and set `http.Client.Timeout` (retain 30s).
- No actor/thread hazards introduced on the engine side; the service is fully self-contained.

### 3.9 Reuse vs. new code

Reuse: `vergex` (models/parsers/formatters/symbol-tiering) as the shared contract; `provider/hyperliquid` client + `fetchPerpDexCoins` pattern for HL info calls; `nofxos` shape/formats as the net-flow/OI/price reference. New: only the service core (ingest/compute/http/config) + any new richer HL info structs. Do not fork the vergex formatters or the netflow formatter.

---

## 4. File-by-file impact

**`provider/vergex/client.go` (modified)**
- Remove `privateKey` field and x402 signing from `doGET`; `doGET` → plain HTTP GET to `c.baseURL+path+query` with `X-Client-ID: nofx`, ctx-aware.
- `NewClient(baseURL string, logger mcp.Logger)` — remove the `privateKeyHex` param and `CLAW402_WALLET_KEY` requirement; baseURL falls back to `SIGNAL_SERVICE_BASE_URL` then `http://localhost:8480`; never errors on credentials.
- Keep the four method signatures, `ParseSignalRanking`, `Filter*`, all symbol/`marketType` helpers, `Format*` unchanged (byte-parity contract). `GetFlowMarkets` retargets to the service netflow path. Drop the `payment` and `crypto` imports; add `nofxos` shape only if `GetFlowMarkets` is parsed (it is not — keep raw passthrough).
- Why: this is the single seam that drops claw402 while preserving every downstream shape/format.
- Dependency: must land atomically with §3.3 service endpoints so `doGET`'s new target answers with compatible JSON.

**`provider/vergex/client_test.go` (modified — additive)**
- Existing tests must pass unchanged (proves parsers/formatters unchanged). Add table-driven tests asserting `NewClient("")` succeeds without a key, and that a 404 containing `"market not found"` and a 400 containing `"invalid markettype"` map to retryable errors (validates the error surface feed for `isRetryableVergexDetailError`).
- Why: guard the transport rewrite and the new error contract.

**`provider/hyperliquid/coins.go` (modified — additive)**
- Extend the (private) `assetCtx`/`metaResponse` or add an exported richer `MetaAndAssetCtxsInfo` type that decodes `openInterest`, `funding`, `fundingRate`, `oraclePx` alongside existing `DayNtlVlm/MarkPx/PrevDayPx`; keep `get...` exporting the list of assets+factors used by the service.
- Why: the service needs OI/funding/OI-delta per cross-section; current structs drop them.
- Dependency: consumer is the service (`ingest.go`); independent of engine changes.

**`service/signal/{service,config,ingest,compute,http,pineify}.go` + `cmd/signal-service/main.go` (new)**
- The standalone service as per §3.2–3.3, §3.5–3.6. Imports `nofx/provider/vergex` (symbol helpers + models for response marshalling to guarantee shape symmetry), `nofx/provider/hyperliquid` (info calls).
- Why: the self-hosted data plane; separate binary/container per user decision.
- Dependency: `compute.go` needs the richer HL structs from `coins.go`; `http.go` needs `vergex` unchanged.

**`Dockerfile.signal` + `docker-compose.signal.yml` + env example (new)**
- Small binary container; compose service `signal-http:8480`; healthcheck `GET /v1/health`; env wiring. `PINEIFY_MCP_TOKEN` referenced as env only.
- Why: standalone-container requirement.

**`kernel/engine.go` (modified)**
- `NewStrategyEngine`: delete `client.SetClaw402(...)` and the wallet-key-gated construction of vergexClient; always construct `vergex.NewClient(SIGNAL_SERVICE_BASE_URL, logger)`. The `claw402WalletKey ...string` param is retained (ignored) to avoid touching `auto_trader.go`/`api/strategy.go` call sites.
- Later (optional, phase-2): route `FetchNetFlowRankingData` to the service. Phase-1: leave nofxos direct (free, already gated off for HL-native).
- Why: repoint + zero-claw402. Dependency: vergex.NewClient signature change must land with this.

**`kernel/engine_prompt.go` (modified)**
- Add the disclosure line to `vergexHoldRules()` (both zh/en prompts). Nothing else.
- Why: proxy labelling. Safe vs `engine_prompt_test.go` (Contains-based).

**`api/strategy.go` (modified — corrected from "unmodified" per review)**
- `handleStrategyTestRun` currently calls `resolveStrategyDataWalletKey(userID, req.AIModelID)` (api/strategy.go:540), which returns an error → 400 **before** `NewStrategyEngine` is built when no `Claw402WalletKey` is configured. Post-repoint no wallet key is needed, so **relax/remove this resolver gate** so the test-run endpoint works keyless. `NewStrategyEngine(config, claw402Key)` call site is otherwise unchanged.
- Why: the plan's earlier "api/strategy.go untouched" claim was wrong; this file must be edited for the zero-key repoint to work.

**`trader/auto_trader.go` (unmodified, plus optional degradation)**
- `NewStrategyEngine(config, claw402Key)` and `engine.FetchVergexDataBatch(...)` call sites are unchanged, so no edits. Add a tolerant branch in the `Run` loop `GetCandidateCoins` error handling if verification shows it currently aborts (validate first).

**No changes to:** `store/strategy.go` (no schema migration; `GetDefaultStrategyConfig`/`ClampLimits` stable), `mcp/payment/x402.go` (remains for AI LLM client; no longer called from data path), `kernel/engine_analysis.go`, `auto_trader_force.go`, `nofxos/*`.

---

## 5. Risks and migration

- **Semantic downgrade (heatmap/book/net-flow are proxies).** Highest risk: the AI reasoning over proxy liquidation levels could misplace stops. Mitigated by §3.7 disclosure + §3.3 anchors from the real wallet. If the user later obtains a shared full-book source, only `compute.go` changes; the wire contract is stable.
- **Pineify coverage — US equities/options full; crypto = technicals-only (tested).** Pineify supports ~20 Binance USDT crypto majors via `get-technical-analysis-snapshot` (300-field), but no crypto ratings/research/flow. Crypto technical augmentation is optional and limited to those majors; the long-tail HL universe and HL-native signal computation (funding/OI/heatmap) stay self-built. Gated behind `PINEIFY_MCP_TOKEN`, rate-limited to `PINEIFY_RATE_PER_MINUTE` in the ingest worker.
- **No historical backfill.** `clearinghouseState` is per-wallet current-only, so true cost-basis cannot be backfilled for the population. Phase-1 ships current-snapshot proxies; a rolling self-wallet position log (append to memory/disk each poll) enables a shallow history later. No schema migration.
- **Migration/rollback.** No persisted-data or DB schema changes → rollback is revert-and-restart (old code + claw402 key present). The wire contract is backward/forward compatible because the parsers are unchanged; the only invariant is that the service emits the vergex shapes (`bins`/`dimensions`/`SignalRankingData`), validated by the ported `client_test.go` fixtures.
- **Naming:** `vergex` package retains vendor branding though it no longer calls Vergex; acceptable, flagged for a future non-behavioral rename.

Unknowns to validate: (1) whether `metaAndAssetCtxs` exposes `openInterest`/`funding` on the exact decode structs — validate by curling `api.hyperliquid.xyz/info` in staging; (2) `Run` loop's tolerance of a `GetCandidateCoins` error — inspect `auto_trader.go` `Run`; (3) Pineify tool catalog — MCP tool probe.

---

## 6. Implementation order

1. **HL richer info structs** (`provider/hyperliquid/coins.go`) — additive, testable in isolation against a live `info` response. No consumers yet.
2. **Service core** (`service/signal/*`, `cmd/signal-service/main.go`): config → ingest → snapshot → rank/lab/heatmap/netflow compute → HTTP. Compile + unit tests on synthetic fixtures (§7). Container files (`Dockerfile.signal`, compose, env example).
3. **vergex transport rewrite** (`provider/vergex/client.go` `NewClient`/`doGET`) + additive tests. **Must land atomically with step 2's endpoints** (the engine's first service call needs the service contract live). Keep `client_test.go` green.
4. **Engine repoint** (`kernel/engine.go` `NewStrategyEngine`: drop `SetClaw402`, always build vergex client at `SIGNAL_SERVICE_BASE_URL`; keep signature).
5. **Prompt disclosure** (`kernel/engine_prompt.go` `vergexHoldRules`).
6. **Degradation hardening — UNCONDITIONAL.** With `vergexClient` always non-nil (required by the design), a down service makes `getVergexSignalCoins` error in *every* deployment, where before it failed only with "requires a configured claw402 wallet". `api/strategy.go` `handleStrategyTestRun` currently returns 500 on `GetCandidateCoins` error. **Prerequisite: first verify `auto_trader.go` `Run`'s tolerance of `GetCandidateCoins` errors** (does it abort the cycle or manage-positions-only?), then add the tolerant branch so the `Run` loop and the test-run endpoint degrade instead of aborting.
7. **Pineify integration** — optional, after the §2.6 probe (done: full US equities/options + crypto technicals for ~20 majors). Behind env token (`PINEIFY_MCP_TOKEN`); never blocks steps 1–6. Reuse the existing MCP client framework under `mcp/provider` + `mcp/intro` rather than hand-rolling a new MCP client in `pineify.go`. **Scope = `xyz:` TradeFi universe + optional crypto majors**: use `find-ai-stock-picks`/`get-ai-stock-rating` for xyz ranking enrichment, `research-stock`/`get-technical-analysis-snapshot` for Signal-Lab enrichment, `get-market-tide`/`get-sector-flow-snapshot` for net-flow enrichment; for crypto majors use `get-technical-analysis-snapshot` only (map `BTCUSDT`→HL `BTC`). **Mandatory rate limit:** all Pineify calls run inside the single ingest worker throttled by `PINEIFY_RATE_PER_MINUTE` (default 3), writing into the snapshot; never on the HTTP request path or the trading loop. Long-tail HL crypto and HL-native funding/OI/heatmap computation never call Pineify.

Steps **2+3+4** are the atomic unit and must land as one commit: step 3 changes `NewClient`'s signature from `(baseURL, walletKey, logger)` to `(baseURL, logger)`, and `kernel/engine.go` `NewStrategyEngine` currently calls `vergex.NewClient(claw402URL, walletKey, ...)` — so `engine.go` stops compiling the moment step 3 lands and only compiles again after step 4 rewrites that call. Steps 1,5,6,7 are independently compile/testable; step 4 is NOT independently compileable after step 3.

---

## 7. Verification

- **Service unit tests** (`service/signal/*_test.go`): feed fixture `clearinghouseState` (from `cmd/e2e_builder_fee` shape, **plus the added `liquidationPx`/`entryPx` fields**) + fixture `metaAndAssetCtxs` (openInterest/funding/mark); assert (a) ranking `SignalRankItem` fields match the `client_test.go` accepted shapes, (b) heatmap `bins[]` contain exactly `bucketStartPrice/bucketEndPrice/px/longCost/shortCost/longLiq/shortLiq` + `binStep`, (c) netflow matches `nofxos.NetFlowRankingData` keys, (d) empty-instrument and stale cases return 200-empty / 503 respectively. **Pin the computed numbers, not just shapes:** assert the actual z-score composite, the sign of `score` (drives `DirectionalCandidates`/`auto_trader_force` gate `|score| >= 0.4`), `confidence = |z|/maxAbs`, the 1-based rank ordering, OI-delta handling, and the "universe std=0 → all z=0" edge case. **Additional asserts per review:** (i) `Bias` is one of the exact tokens `bullish`/`bearish`/`neutral` (never a sign); (ii) the ranking returns **both bullish and bearish items** when the cohort is directionally split; (iii) heatmap `longCost/longLiq` are **USD-scale** (decode via `FormatHeatmapMarkdown` renders `$K/$M`, matching the `"$1.20M"` fixture); (iv) the `liquidationPx`/`entryPx` struct fields decode correctly; (v) the `liqBand→binStep` mapping is applied (e.g. `"15"`→1×, `"5"`→0.5×).
- **Shape/format parity tests**: marshal service output and run it through `vergex.ParseSignalRanking` + `vergex.FormatAnalysisForAI`; assert `FormatSignalLabMarkdown`/`FormatHeatmapMarkdown` produce the same headers/rows as the existing `client_test.go` fixtures (golden-markdown comparison). This proves byte-compatible prompts.
- **Engine integration (no behavior drift)**: point `vergex.NewClient` at an `httptest.Server` serving the fixture JSON; the untouched `engine_prompt_test.go` (`TestBuildSystemPromptUsesVergexClaw402Prompt`), `engine_directional_test.go` (`TestDirectionalCandidates...`), and `engine_vergex_test.go` must all still pass. Optionally add one test driving `enrichVergexDataWithStrategy` through the httptest server to prove `VergexDataMap` populates.
- **E2E prompt-equivalence**: run the service locally; run `api/strategy.go` `handleStrategyTestRun` with a `vergex_signal` config; diff the `system_prompt`/detailed-data section against a pre-change golden, expecting only the added §3.7 disclosure line to differ.
- **End-to-end smoke**: start container, hit `/v1/health`, `/v1/signal/ranking`, `/v1/signal/lab?symbol=BTC`, `/v1/signal/heatmap?symbol=BTC`, `/v1/netflow/ranking`; kill the container → confirm engine continues on existing positions without panic.

---

## Open Questions (remaining after review decisions)

1. **Pineify crypto coverage — RESOLVED (tested): technical-analysis only for ~20 Binance USDT majors** (BTC/ETH/SOL/…). No crypto ratings/research/flow. Decision: optionally enrich those majors' ranking with Pineify technicals (map `BTCUSDT`→HL `BTC` core_perp); long-tail HL + HL-native computation stays self-built. No further probe needed.
2. **Candle-based ATR / 1h-4h deltas** — deferred enhancement (per review decision); v1 ranking uses free `prevDayPx` 24h delta + funding + OI-delta, and `binStep` uses a `prevDayPx`-based volatility proxy. Revisit only if validation shows the proxy is too crude for the heatmap.
3. **`Run` loop tolerance** — does `GetCandidateCoins` error currently abort a cycle (manage-positions-only)? **Required prerequisite for work item 6** (which is UNCONDITIONAL because a down service now errors every cycle in every deployment, and `vergex_signal` is the default source). Verify `auto_trader.go` `Run` before claiming step 6 done.
4. **Shared nofxos credential — largely MOOT for this plan (review-verified).** For `vergex_signal`, `usesHyperliquidNativeUniverse()` gates all three nofxos ranking fetches to nil, so `nofxosClient` is never called in the primary path; the shared `DefaultAuthKey` risk does not fire for vergex_signal (only matters for non-HL-native source types).
5. **Backfill scope** — current-snapshot proxies in phase 1; shallow self-wallet history later (rolling position log). No schema migration.

### Resolved decisions (from review — no longer open)
- Mixed-universe z-scores → **per-cohort** (`core_perp` vs `hip3_perp`), merged into one board.
- `liqBand→binStep` → fixed table: `""`→1×, `"5"`→0.5×, `"10"`→0.75×, `"15"`→1×, `"20"`→1.5×.
- Heatmap USD scaling → pool = `OI × mark`, distributed by pressure proxy; renders `$K/$M`.
- Bias emission → exact engine tokens `"bullish"/"bearish"/"neutral"` (+ aliases), never `sign()`.
- v1 factors → `prevDayPx` delta + funding + OI-delta only (no candle/ATR dependency).
- `liquidationPx`/`entryPx` → must be added to a `clearinghouseState`-shaped struct (flagged field addition).
- `estimatedMargin` → removed from the exposure formula.
- Net-flow parity → API-complete-only in phase-1; engine stays on free nofxos.
- `api/strategy.go` → MUST be edited to relax `resolveStrategyDataWalletKey` (zero-key repoint).

## 8. Paid Claw402 vs Free Signal-Service: Parity & AI-Sufficiency

> **Analysis status:** appended after live comparison of captured claw402 responses (signal-ranking, heatmap, flow-markets) against the self-hosted service output. Analysis-only; no code changed by this note.

**Executive summary — only a small sub-surface of the paid payload is actually consumed by the AI.** The formatters (`FormatAnalysisForAI` → `FormatSignalLabMarkdown`/`FormatHeatmapMarkdown`) and the engine read a narrow field set (`bins`, `dimensions`, `SignalRankItem`). The free service reproduces **all consumed fields**, so prompt output is near byte-equivalent. But **"field present" ≠ "value faithful"**:

| Product | Consumed fields present? | Value semantics faithful? | Verdict |
|---|---|---|---|
| Signal ranking | ✅ | ⚠️ bias = momentum, not crowdedness | **Sufficient** (caveats) |
| Signal lab | ✅ | 🔴 3 near-vacuous proxy rows, no percentile | **Not sufficient** as "core confirmation" |
| Cost/liq heatmap | ✅ | 🔴 longLiq/shortLiq fabricated → stops/targets anchored on them | **Not sufficient** for absolute levels (highest risk) |
| Netflow | ⚠️ API-only | 🔴 proxy (~34K vs paid ~2.56M) | **N/A** phase-1 (unconsumed) |

**Key finding:** Claw402's "richer" fields (`raw.cgoPct`, `lfaPct`, `cascadeVuln`, `markPriceSource`, `market{isActive,marketId}`, `cost{state,totalPositions}`, `liquidation{state,reason}`, `meta{asOfBlock,appliedThroughEventId}`) are **never parsed or rendered** by any parser/formatter — dropping them causes **zero prompt regression**. The real risk is the free products' *fabricated values* for consumed fields, because the AI **acts** on them.

### 8.1 Signal Ranking — SUFFICIENT
- All consumed fields present; `bias` tokens exact; ordering real; `|score|≥0.4` gate runs on a genuine per-cohort z-score. ✅
- **Caveat (P2):** free `Score` = momentum+funding+OI only. Paid `compositeZ` incorporated concentration/leverage/cascade-vulnerability (`cgoPct`/`lfaPct`/`cascadeVuln`). A fragile, OI-driven move can get a clean bullish/bearish tag with no crowding penalty — that signal is gone entirely.

### 8.2 Signal Lab — NOT SUFFICIENT as "core pre-entry confirmation" (P1)
- Schema present (`dimensions[]`), but only 3 proxy rows: **POC, 24h Momentum, Funding Pressure**. POC-direction ≈ momentum-direction; funding already in ranking → rows carry ~no incremental info. Real technicals (RSI/EMA/ADX/levels/event) are gone. `percentile` renders as `-`.
- **Impact:** the rule *"Open only when Signal Lab, heatmap and raw candles broadly agree"* becomes ~vacuously satisfied whenever momentum agrees with ranking — the confirmation gate is effectively removed.

### 8.3 Cost/Liq Heatmap — NOT SUFFICIENT for stop/target levels (P0)
- Every field the AI sees is present and USD-scaled; **values are synthetic** — `longLiq/shortLiq` are a deterministic exp-taper (pegged 33% of mark cost), not real liquidation clusters.
- The prompt instructs the AI to place stops/targets at "heatmap resistance/liquidation zones"; it therefore anchors a stop/target to a **fabricated** level believing it is a real crowd boundary. §3.7 disclosure ("stress indicators") partially contradicts this operational rule.
- **Recommendation (documented, not implemented):** reposition the free heatmap in the prompt from "stop/target zones" to "relative stress/volatility context," with stops/targets derived from ATR/candle structure instead.

### 8.4 Netflow — N/A phase-1 (unconsumed)
- Gated off for `vergex_signal` (`usesHyperliquidNativeUniverse`); not rendered. Structural parity on the emitted shape; **magnitude/semantics are a proxy** (funding×OI×mark, ~34K vs paid 2.56M real taker-flow). No current impact.

### 8.5 Cross-cutting residual risks
| # | Risk | Severity | Affected decision |
|---|---|---|---|
| R1 | Fabricated heatmap liq → stop/target on synthetic zones | **P0** | Stop/target placement |
| R2 | Vacuous Signal Lab → confirmation gate collapses | **P1** | Entry confirmation |
| R3 | Ranking lacks crowding/fragility factor | **P1** | Direction bias of universe |
| R4 | `confidence` semantics changed (relative-max vs absolute) | **P2** | Confidence weighting |
| R5 | Contract gap: plan §3.5/§7 claim `liqBand→binStep` mapping, but `handleHeatmap` parses only `symbol` (ignores `liqBand`) | **P1** | Heatmap scaling differential |

**Net judgment:** Ranking is faithful and safe to depend on. Signal Lab and Heatmap reproduce the *schema* but not the *semantics* — the free products the AI leans on for confirmation and stop/target placement are proxies or fabricated. **R1 (fabricated liquidation feeding stop/target logic) is the single highest residual risk** to real trading decisions.

### 8.6 Signal Matrix selection path (dashboard)

The dashboard's Signal Matrix is populated from **`/api/vergex/signal-ranking?marketType=all&limit=30`** (i.e. `SignalRankingPath`), not a dedicated endpoint.

- **Chain:** `TerminalDashboard` → `api.getSignalRanking(..., marketType='all', limit=30)` (polled ~5m) → backend `handleVergexSignalRanking` → `vergex.GetSignalRanking` → `FilterSignalRankingItems(items, 'all', 30)` → `SignalMatrix` renders `items` (top **18** by `rank`).
- **On dev (paid):** the returned 30 are claw402's proprietary `compositeZ` ranking of the whole universe; the matrix shows its top 18 (e.g. NBIS/CXMT/SKHY/NVDA…).
- **On feature (free):** the same endpoint serves our per-cohort **z-score** ranking instead — a different symbol set/order. This is the §8.1 parity gap (no crowding/leverage/cascade factor), not an API-shape difference: the wire shape and matrix rendering are identical, only the ranking *values* differ.

---

## References

- Hyperliquid info API: https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals
- Pineify MCP: `https://agents.pineify.app/mcp` — bearer auth; token = env `PINEIFY_MCP_TOKEN` (never committed). **Tested: full US equities/options toolset + technical-analysis for ~20 Binance crypto majors** (BTC/ETH/SOL/…); no crypto ratings/research/flow. Tool→signal mapping in §2.6. Rate-limited (~few/min).
- NOFX seams: `provider/vergex/client.go`, `provider/vergex/client_test.go` (fixtures), `kernel/engine.go`, `kernel/engine_prompt.go`, `kernel/engine_analysis.go`, `store/strategy.go`, `provider/hyperliquid/coins.go`+`kline.go`, `provider/nofxos/*` (netflow/oi/price shapes), `api/handler_hyperliquid_wallet.go:81`, `cmd/e2e_builder_fee/main.go`.
