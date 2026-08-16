# NOFX Deep Architecture Review

Date: 2026-08-16 · Branch: `feat/self-hosted-signal-stack`

This is a complete how-the-system-works reference, grounded in `file:line`. It covers every major flow end-to-end. Use it as the authoritative map before modifying any subsystem.

---

## 1. System topology

Three processes + SQLite, deployed via docker compose:

| Process | Role | Listen |
|---|---|---|
| **`nofx`** (Go) | HTTP API + trader runtime + strategy engine + AI model layer | :8080 |
| **`signal-service`** (`service/signal`, Go) | self-hosted data plane (ingest / rank / lab / heatmap / flow) | :8480 |
| **`nofx-frontend`** (React/Vite) | Web UI (terminal, traders, onboarding, strategy studio) | :3000 |

Persistence: **SQLite** (`data/data.db`, GORM), optional Postgres. Every trader in the DB is loaded into memory at boot and auto-started if `IsRunning`.

**Boot** (`main.go:24`): CLI short-circuit (`cli.go` — account recovery is CLI-only, never a network endpoint) → `.env`/config → crypto service → `store.NewWithConfig` (`:77`) → `auth.SetJWTSecret` (`:91`) → `manager.NewTraderManager` + `LoadTradersFromStore` (`:103`) → `api.NewServer` (`:135`) → `server.Start()` goroutine + Telegram bot goroutine → graceful shutdown on SIGINT/SIGTERM.

---

## 2. Backend: API surface & auth

`api/server.go` `setupRoutes` registers all `/api` routes via `route_registry.go` (feeds LLM API docs). Three groups:
- **Public**: `/health`, `/supported-models`, `/supported-exchanges`, klines, symbols, competition/equity, hyperliquid connect-config, `/strategies/public`.
- **Auth (rate-limited)**: `/register`, `/login`.
- **Protected (JWT)**: onboarding, users, models, exchanges, traders (+ start/stop/preflight/close-position/sync-balance/grid-risk), strategies, vergex/signal, per-trader status/account/positions/decisions/statistics, AI-costs, telegram.

**Auth** (`auth/auth.go`): HS256 JWT, 24h TTL, issuer `nofxAI`, claims `{user_id,email}`; strict signing-method check; bcrypt passwords; `authMiddleware` (`server.go:470`) checks Bearer + in-memory blacklist (expiry-swept, 100k cap). Rate-limit on auth only. **IDOR hardened**: `getTraderFromQuery` (`server.go:329`) resolves `trader_id` only from the caller's owned store list.

---

## 3. Store / DB layer

GORM, SQLite default / Postgres optional (`store/gorm.go:34/76`). `Store` facade (`store/store.go:15`) with lazy sub-stores: `User/AIModel/Exchange/Trader/Decision/Position/Strategy/Equity/Order/Grid/AICharge/TelegramConfig`. Notable:
- `AIModel.APIKey`, exchange secrets = `crypto.EncryptedString` (field-encrypted at rest).
- `DecisionRecord` JSON-serializes candidate/execution-log/decision arrays; `TraderOrder`/`TraderPosition` use int64-ms UTC; `EquitySnapshot` uses time.Time.
- `StrategyConfig` custom `Marshal/UnmarshalJSON`; `RiskControlConfig` (`store/strategy.go:931`) + `GetDefaultStrategyConfig` (`:980`) + `ClampLimits` (`:40`).

---

## 4. Data plane — `service/signal`

**Routes** (`http.go`): `/v1/health`, `/v1/products`, `/v1/signal/{ranking,lab,heatmap}`, `/v1/netflow/ranking`, `/v1/signal/priority`.

**Ingest** (`service.go` `Ingest`, every `SIGNAL_SERVICE_INTERVAL`=3m): `ingestDex` (core_perp + hip3_perp) pulls `metaAndAssetCtxs`, **excludes delisted assets** (our fix — `tradableSnapshot`: `!IsDelisted && MarkPx>0`), optional rate-limited **Pineify** overlay (prioritized by `/v1/signal/priority` candidates), then swaps the snapshot carrying OI-delta + Pineify forward.

**Rank** (`compute.go:33`): per-cohort z-scores (price24h, funding, OI-delta) → composite → bias/confidence board. **SignalLab**: on-demand per-symbol dimensions. **Heatmap**: flow-driven cost (real WS taker-flow via `decayedBin`) + synthetic liq ladder, stop-risk tuned. **NetFlow**: institution=funding×OI×mark, retail=OI-delta×mark.

**WS** (`hlws.go`): gorilla/websocket to Hyperliquid `trades`+`l2Book`; per-price-bin notional with 30m half-life decay / 2h max-age; lazy subscribe to priority + touched symbols, 30s reconcile, backoff reconnect.

---

## 5. Kernel / strategy engine + AI decision flow

`GetCandidateCoins` (`engine.go:277`) branches on `coinSource.SourceType` (static/ai500/oi_top/hyper_rank/vergex_signal/mixed), all ending in `filterExcludedCoins`. `vergex_signal` → `getVergexSignalCoins` (`:698`): fetch ranking fresh, rebuild `vergexRankingCache`, interleave bull/bear/other so the universe carries both directions.

`GetFullDecisionWithStrategy` (`engine_analysis.go:96`):
1. **Token-estimate gate** vs the specific model's context limit (`GetContextLimitForClient`); blocks on overflow.
2. Prune candidates without market data; enrich vergex data.
3. Build system+user prompts (`engine_prompt.go`).
4. **AI call** — 2 retries only on transient empty-content (`isTransientEmptyContentError`).
5. **`parseFullDecisionResponse`** → `extractCoTTrace` + `extractDecisions` (see §9 deep mechanics) → `validateDecisions`/`validateDecision` (`engine_position.go:48`).

`validateDecision`: tiered leverage/position-ratio (BTC/ETH + xyz = high tier, else altcoin), min-size (12/60), position-value cap, SL/TP ordering per side, **R:R ≥ 3.0**, and the **stop-risk cap** (`RiskCappedPositionSize`, stop-out ≤ `RiskPerTradePct%` of equity).

---

## 6. Trader execution + position lifecycle

`runCycle` (`auto_trader_loop.go:22`, every `ScanInterval`=15m):
1. build context (balance, positions, peak-PnL, candidates, recent trades/stats, quant) → `saveEquitySnapshot` (tracks `peakEquity`).
2. AI decision → persist CoT/prompts/raw → record AI charge.
3. `consecutiveAIFailures`: ≥3 → **safe mode**; `ErrInsufficientFunds` → mark wallet empty.
4. sort (close→open) → `filterDecisionsToStrategyUniverse` → `ensureLongShortCoverage` (deterministic top-up via `DirectionalCandidates`) → safe-mode filter.
5. per-decision **throttle** + **correlation block** → `executeDecisionWithRecord`.

**Execute open** (`auto_trader_orders.go:110/230`): `enforceMaxPositions` → balance/equity → `applyAutopilotFullSizeOpen` + `enforcePositionValueRatio` (drawdown-scaled) → margin auto-reduce → min-size → `SetMarginMode` → `OpenLong/Short` → `recordAndConfirmOrder` → **`attachStopLossTakeProfit`** (fatal on error — position left unprotected).

**Position sync**: exchanges with OrderSync (`binance/order_sync.go`, `syncloop.Run`, incremental by trade ID) skip the inline poll; others use `recordAndConfirmOrder` 5× poll. Rebuild via `RebuildPositionsFromTrades` (FIFO, dust epsilon). Drawdown monitor (1-min, +5% arm / 40% giveback) + emergency close.

---

## 7. Risk layers (all code-enforced, model-independent)

per-position `equity×ratio` → **stop-risk sizing** (stop-distance cap, `RiskPerTradePct`=3) → **drawdown trim** (0.5×/0.25× below 15%/30% of peak) → **correlation block** (≥0.7 corr with a held position) → throttle (per-cycle 2 / per-hour 3) → min-hold 90m / noise band 3h / re-entry cooldown 4h → margin cap 0.5 + auto-reduce → `MaxPositions=2`.

---

## 8. Frontend flows

**SWR polling at page/route level → prop-driven panels.** `AppRoutes` polls traders/status/account/positions/decisions (5–30s, on-error backoff toggles poll-off). `TerminalDashboard` fetches full-stats/history/config/flow/signal-rank (flow+signal at `VERGEX_TERMINAL_REFRESH_MS`=3m). `LiquidationMap` tries hip3→perp fallback; `OrderBook` uses Hyperliquid WS; `KlineChart` polls 60s. `useDemoEngine` (Shift+D) swaps every dashboard dataset client-side. Trader CRUD centralized in `AITradersPage` with dumb modals; `httpClient` centralizes auth headers, 401 handling, RSA transport encryption for sensitive fields.

---

## 9. Deep mechanics (verified firsthand)

- **Decision parsing robustness** (`engine_analysis.go:360-487`): `fixMissingQuotes` normalizes smart quotes + full-width CJK punctuation; `validateJSONFormat` rejects `~` ranges and thousand-separator commas; requires `[{` start; **safe fallback** to `{ALL, wait}` if the model emits no structured JSON (degrades to wait, not crash).
- **Auth**: strict HMAC alg check; in-memory blacklist with expiry sweep + 100k cap.
- **Entry security**: account recovery is CLI-only (no network path).
- **Priority push**: trader POSTs candidates to `/v1/signal/priority` so ingest prioritizes Pineify on exactly the traded symbols.
- **Markets**: klines from CoinAnk (WS monitor disabled); Hyperliquid used for perp/xyz data + WS flow.

---

## 10. End-to-end flows

- **Trading cycle (15m)**: Hyperliquid → signal-service (3m) → `GetCandidateCoins` → prompt → AI (deepseek-v4-flash via custom endpoint) → validate/risk-gate → execute + SL/TP → positions → OrderSync → DB.
- **Terminal render**: page SWR → `TerminalDashboard` → panels; heatmap/flow/signal at 3m, book via WS.
- **Trader management**: `AITradersPage` → create/start → `runLaunchPreflight` → `handleStartTrader` gates on `Ready` unless `?force=true`.
- **Onboarding**: `POST /onboarding/beginner` → create/adopt AI wallet → save model → persist `.env`.

---

## 11. AI prompt construction (what the model sees) — `kernel/engine_prompt.go`

The autopilot uses `buildVergexSystemPrompt` (en/zh). Structure: **schema guide → role → data priority → trading rules → hold rules → hard constraints → output format → custom prompt**. Anti-churn & fee-aware by design:
- `vergexHoldRules`: hold ≥90m, never close inside −2%..+3% noise before ~3h, 4h re-entry cooldown, ≤1–2 opens/hour; round trip ≈0.1% fees → only targets beyond fees (stop ≈−3%, target ≈+8%+); forbids 0.2–0.3% scalps.
- Hard constraints: max positions, notional = `equity × ratio`, margin ≤50%, min size, fixed leverage, R:R ≥3, min confidence, and the **stop-risk cap** ("a wider stop yields a smaller position").
- Data priority: ranking → Signal Lab → heatmap → candles. Signal Lab **Flow/Liquidity is flagged REAL** (WS taker/order-book), but stops/targets always come from ATR + candle structure; heatmap/net-flow are labeled **self-built proxies** (never anchor a level on them).
- Output contract: `<reasoning>`/`<decision>` XML + strict JSON schema (exact fields, "all numbers calculated not formulas", symbols must match candidates/positions).
- Single-symbol XYZ variant adds long/short entry conditions + guardrails (1.5–3% stop, ≥2:1, ≤25% notional, 2–3x leverage, no flip-in-same-cycle).

## 12. Hyperliquid execution layer — `trader/hyperliquid/`

- **Agent-wallet security** (`trader.go:161-300`): private key = agent wallet (signing only, ~0 balance), main wallet holds funds; refuses main-wallet-as-agent and agent balances >10/>100 USDC; constructor panics recovered via `initExchangeClient`.
- **Precision** (`trader.go`): `roundPriceForOrder` enforces both ≤5 sig-figs AND ≤`6−szDecimals` decimals — snaps low-priced coins (e.g. HEMI) to the per-symbol tick `10^-(6−szDecimals)`.
- **Orders** (`trader_orders.go`): `placeOrderWithBuilderFee` uses an approved builder (5bps, `0x891d…`) with fallback retry; `OpenLong/Short` cancel pending → `SetLeverage` (abort-on-failure, direct xyz action) → order (aggressive 1.01/0.99) → wire-format size. **xyz orders** bypass SDK `NameToAsset`, construct+sign `OrderAction` directly (HIP-3 asset index).
- **SL/TP**: reduce-only triggers; `isBuy = positionSide=="SHORT"`.

## Known watch-items (candidate follow-ups)

1. `attachStopLossTakeProfit` is fatal — SL placement failure leaves a position unprotected.
2. Whole-cycle abort on one invalid decision (measured: 0 occurrences in 121 cycles — low priority).
3. `ensureLongShortCoverage` forced opens carry SL/TP=0 until defaults attach.
4. claw402 provider remains unused (deferred removal).
