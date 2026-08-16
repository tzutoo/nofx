# AI Decision Profit & Drawdown Optimization: Plan

Date: 2026-08-16 · Branch: `feat/self-hosted-signal-stack`

## Goal

Make the NOFX autopilot's AI-decision flow as **profitable as possible while reducing drawdown** — but the single highest-leverage finding is that the reported drawdown is **untrustworthy**, so the work is sequenced as: **(1) make equity/drawdown measurement sound, (2) add real drawdown management, (3) fix AI-call reliability, (4) selectively enrich the AI signal context.**

## Executive summary

The system is profitable and reliable at the execution layer (+23.7 realized PnL, 94.3% decision success, 100% order fill) but the reported **36.6% equity drawdown is almost certainly a measurement/restart artifact, not a real trading loss**: equity fell 157.7→100.0 in under an hour coinciding with a backend restart, while the realized PnL only explains a worst single loss of −0.42. The plan therefore does **not** start by "cutting drawdown" against phantom data — it first proves/repairs the metric, then adds drawdown controls, then reliability, then richer signals. ~81-day average holds mean large unrealized swings are the real (under-measured) risk driver.

---

## Part A — Current-state analysis

### A1. Measured reality (live SQLite `data/data.db`, 2026-08-13→08-16)
- Realized PnL **+23.7** / 15 closed positions; avg +1.58; worst single realized loss **−0.42**. Single trader.
- Decision success **94.3%** (115/122). Order fill **100%** (47/47).
- **Suspicious drawdown**: equity max **157.67 → 100.0** = 36.6%. The drop is **instantaneous (00:22→01:19 UTC)**, coinciding with our backend redeploy/restart (09:11–09:19 +0800). Realized PnL does not account for a −57 move.
- **Holds ~81 days avg** (min 2.5h, max ~260d) → large unrealized mark-to-market swings dominate equity, not realized edges.
- **AI reliability**: 7/122 decision failures — 4× **SSE "no content received" (~2.5MB response dropped)**, 3× provider 500.

### A2. AI context (already rich; everything on the signal surface is wired in)
`kernel/engine_prompt.go:771` `BuildUserPrompt` feeds: system status, BTC market, account/balance/margin, recent trades, historical stats (profit factor/Sharpe/win-rate/drawdown), positions (per-symbol + market/quant/vergex), candidates (same per-symbol enrichment), OI ranking, NetFlow ranking, price ranking. Per-symbol: price, EMA20, MACD, RSI7, ATR14, BOLL, klines, funding, OI, quant netflow/OI-delta, and `formatVergexData` (ranking + SignalLab incl. **real WS taker-flow** + order-book imbalance + Pineify rows + heatmap totals/top-10).

### A3. Available-but-compressed signal headroom
- Full **heatmap liquidation-profile** (only top-10 of ~121 bins passed; `provider/vergex/client.go:472,479`).
- **Per-price-bin flow** (`buyByBin`/`sellByBin`) — only aggregate buy$ vs sell$ passed (`compute.go:flowSignalRows`).
- **Per-bin order-book depth** (`bidScope`/`askScope`) — only aggregate imbalance passed.
- **Pineify overlay depth** — compressed to ≤3 rows.
- OI/funding **history** NOT available (only current + `OIPrev`); kline indicators exist but are **flag-gated** (`Indicators.EnableX`).

### A4. Existing risk layers (all code-enforced)
stop-risk sizing (#1, ≤`RiskPerTradePct`=3% equity), drawdown size-trim (#2, 0.5×/0.25× below 15%/30% peak), correlation block (#4, ≥0.7), throttle (2/cycle, 3/hour), min-hold 90m, noise band 3h, re-entry 4h, margin cap 0.5 + auto-reduce, `MaxPositions=2`, full-size-or-wait (`equity×ratio`=1.0 × 3x). Drawdown auto-close: price-basis +5% arm, 40% giveback.

> **Note on sizing layers:** the "full-size-or-wait" standard path and the drawdown trim (0.5×/0.25×) are in tension — the trim already scales size down in drawdown, so "full size" is not unconditional. WS-2's **binary breaker** stacks on top of the **continuous trim**; make the combined drawdown response a single, comprehensible knob and ensure the model isn't surprised when the trim silently shrinks a size it was told to set at full notional.

---

## Part B — Workstreams (sequenced, each implementation-ready)

### WS-1 — Make equity/drawdown measurement sound (DO FIRST)
**Goal:** prove whether the 36.6% drawdown is real; if it's a restart artifact, fix the metric so future tuning is trustworthy.
- **Diagnose:** reconcile equity snapshots against realized PnL + positions + exchange account balance across the restart window. Determine if the −57 drop is (a) an `initialBalance` recompute on restart, (b) an unrealized-PnL component lost/recomputed, (c) an exchange-side account-value change, or (d) a genuine (unrealized) move.
- **Repair (if artifact):** persist equity baseline/`initialBalance` so a restart cannot reset it; ensure `EquitySnapshot.Save` and the drawdown/`peakEquity` tracking survive restart without phantom drops; add a guard that ignores a snapshot discontinuity > threshold unless corroborated by position/balance changes.
- **Files:** `trader/auto_trader_decision.go` (`saveEquitySnapshot` :24, `peakEquity` :32), `store/equity.go`, `store/trader.go` (`InitialBalance`), `api/handler_competition.go` (equity history).
- **Done when:** the equity curve is provably consistent with realized + unrealized PnL across a restart; reported drawdown reflects reality.
- **Verify:** SQL reconciliation query + a restart test that equity baseline is stable; `go test ./trader/ ./store/`.

### WS-2 — Real drawdown management
**Goal:** once the metric is sound, cap drawdown without gutting profit.
- **Hard drawdown circuit-breaker:** track peak equity; when equity is ≥ X% below peak (e.g. 12%), **pause `open_*` decisions** for a cooldown (or until recovery within Y% of peak). Scoped to opens only — `close_*`/`hold`/management continue to execute, matching the throttle's bypass pattern, and it also gates `ensureLongShortCoverage` forced opens. Reuse the existing `stopUntil` pause mechanism (`auto_trader_loop.go:54`) rather than adding a parallel flag.
- **Daily loss-limit pause:** a max daily P&L that pauses new opens for the rest of the day. **P&L basis must be explicit:** an equity/mark-to-market limit is the real risk driver (81-day holds → unrealized swings) but requires WS-1 (measurement integrity) to be trustworthy; a realized-only limit avoids phantom triggers from the 36.6% artifact but misses the unrealized risk. Recommend equity-based, gated on WS-1; document the chosen basis.
- **Tighter/earlier exits for 81-day holds:** the drawdown auto-close currently arms at +5% then 40% giveback. Consider a **trailing profit-protect** and a **time-based / signal-deterioration exit** so long swing holds don't bleed unrealized gains back. Feed exit guidance into the prompt and enforce via `closeThrottleReason` bypasses.
- **Files:** `trader/auto_trader_risk.go` (breaker + trim), `trader/auto_trader_loop.go` (`stopUntil`, `tradeThrottleReason`), `store/strategy.go:931` (`RiskControlConfig`), `kernel/engine_prompt.go` (`vergexHoldRules`).
- **Done when:** a scripted losing stretch pauses new opens at the configured drawdown/daily thresholds; exits protect realized gains; tests cover breaker + daily-limit + exit bypass.
- **Verify:** unit tests + a dry-run scripted series on the equity/position store; `go test ./trader/`.

### WS-3 — AI-call reliability
**Goal:** eliminate the recurring SSE "empty content" / provider-500 cycle that wastes 15-min cycles and feeds safe-mode.
- **Root cause:** ~2.5MB SSE responses get dropped (empty content) — a response-size limit / stream handling issue. Add a **response-size guard / large-payload retry**. Locate the **actual retry loop** in `GetFullDecisionWithStrategy` (kernel) — the helper `isTransientEmptyContentError`/`emptyContentRetryWait` (`engine_analysis.go:17,32`) feeds it, but the SSE read-size limit that drops ~2.5MB payloads lives in `mcp/client.go` / the SSE reader; verify both before adding a guard. Optionally raise the provider's max_tokens / context window.
- **Files:** `kernel/engine_analysis.go` (`GetFullDecisionWithStrategy` retry), `mcp/client.go` / `mcp/payment/*` (SSE read size limit), provider config.
- **Done when:** no `SSE empty` failures over a monitored window; safe-mode only trips on genuinely unrecoverable errors.
- **Verify:** log/DB failure-rate before vs after; `go test ./kernel/ ./mcp/`.

### WS-4 — Selective richer AI signals (highest cost-benefit)
**Goal:** give the AI the highest-value missing detail with bounded token cost.
- **ATR exposure:** the prompt references ATR but it's flag-gated — surface current ATR value so SL/TP guidance ("from ATR") is grounded in an actual number the model can use.
- **Per-bin flow + depth:** pass flow/depth concentration (e.g. the few price levels where aggressive flow/resting depth cluster) rather than just aggregate buy$ vs sell$.
- **Heatmap liq-profile:** pass the full (or top-N by magnitude) liquidation clusters so the model sees where liq concentrates, not just totals/top-10.
- **Pineify depth:** expose rating score + full event list where available.
- **Files:** `service/signal/compute.go` (format), `provider/vergex/client.go` (`FormatSignalLabMarkdown`, `FormatHeatmapMarkdown`), `kernel/engine_prompt.go` (`formatVergexData` :1034).
- **Done when:** these fields appear in the AI context and the model references them in `<reasoning>`; token cost measured and within an agreed ceiling.
- **Verify:** confirm a **numeric ATR value** (not just the string "ATR") is present in the per-symbol context, plus the flow/liq cluster levels; measure per-cycle token delta; manual check that `<reasoning>` cites ATR/flow/liq **levels**.

---

## Part C — Execution index

| # | Item | Goal | Done when | Key files | Deps | Size |
|---|------|------|-----------|-----------|------|------|
| 1 | Equity/drawdown measurement integrity | Metric is trustworthy across restarts | SQL reconciliation + restart-stable baseline | `auto_trader_decision.go`, `store/equity.go`, `store/trader.go` | — | Med |
| 2 | Drawdown circuit-breaker + daily loss-limit | Pause opens on losing stretch | Scripted test pauses at threshold | `auto_trader_risk.go`, `auto_trader_loop.go`, `store/strategy.go` | 1 | Med |
| 3 | Tighter exits for 81-day holds | Protect realized gains | Trailing/time/signal-deterioration exit tested | `auto_trader_risk.go`, `engine_prompt.go` | 1 | Med |
| 4 | AI-call reliability | No SSE-empty failures | Monitored window clean | `engine_analysis.go`, `mcp/client.go` | — | Small |
| 5 | Richer AI signals (ATR/flow/liq/Pineify) | Model sees high-value missing detail | Fields in context + cited in reasoning | `compute.go`, `client.go`, `engine_prompt.go` | — | Med |

**Order:** 1 → 2 → 3 → 4 → 5. 1 is the gate for 2/3 (don't tune drawdown against bad data); 4/5 are independent.

---

## Part D — Tradeoffs & risks
- **Circuit-breaker vs opportunity cost:** pausing opens after a drawdown avoids deeper losses but can miss recoveries; the cooldown threshold is the tuning knob.
- **Tighter exits vs 81-day winners:** earlier/profit-protect exits lock gains but can cut trend winners short; make it a configurable trailing %.
- **Richer signals vs token cost:** more context = more tokens/cycle and more AI cost + longer decisions; the plan bounds this by adding only ATR + top-cluster flow/liq (not the full ~121-bin grid).
- **Measurement fix risk:** if the "artifact" is actually a real margin/liquidation event, WS-1's reconciliation surfaces it and WS-2's breaker is the correct response — the plan handles both.
- **AI reliability:** raising response-size/max-tokens could increase cost slightly but removes wasted 15-min cycles.

## Part E — Open questions (carried)
1. If reconciliation proves the −57 drop is a real (unrealized) move, which symbol(s) drove it? (user's alt-coin hypothesis) — needs the WS-1 position-overlay diagnosis.
2. Acceptable per-cycle AI token increase for WS-4? (cost ceiling to set before implementation.)
3. Hard-drawdown pause threshold (default 12%) and daily-loss limit (default 8%) — confirm before tuning.

## References
- `docs/reviews/system-architecture-deep-review-2026-08-16.md` — full architecture
- `kernel/engine_prompt.go:771` `BuildUserPrompt`; `:1034` `formatVergexData`; `:197-481` vergex system prompt
- `kernel/engine_analysis.go` `GetFullDecisionWithStrategy` (AI retry on transient empty-content); `mcp/client.go` + SSE reader (response-size limit)
- `service/signal/compute.go` (rank/lab/heatmap/netflow); `hlws.go` (flow/depth)
- `trader/auto_trader_risk.go` (stop-risk, drawdown trim, correlation); `auto_trader_throttle.go`; `auto_trader_loop.go:54` (`stopUntil`); `auto_trader_decision.go:24` (`saveEquitySnapshot`)
- `store/strategy.go:931` `RiskControlConfig`; `store/equity.go`; `store/trader.go` (`InitialBalance`)
- Live SQLite `data/data.db` (equity, positions, decisions)
