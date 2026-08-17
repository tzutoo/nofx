# Sub-Day 15m Scalp TP/SL (ATR-Adaptive): Plan

Date: 2026-08-16 · Branch: `feat/self-hosted-signal-stack` · Plan only — no code, no commit.

## Goal

Replace the fixed swing-style **−3% stop / +8% target / hardcoded R/R≥3.0** exit scheme of the 15m NOFX Autopilot trader with a **per-coin ATR-adaptive sub-day scalp** scheme for altcoins on Hyperliquid: `stop = 1.5×ATR15m`, `target = 2×ATR15m` (real-entry R/R ≈ 1.33), the R/R floor made config-driven (default lowered to ~1.2), and high-volatility coins **hard-excluded** from the 15m pool by an ATR eligibility cap (default 3%).

### Confirmed user decisions
1. TP/SL shape: **stop = 1.5×ATR15m, target = 2×ATR15m (R/R ≈ 1.33)**.
2. R/R floor: **wire `validateDecision` to read `RiskControlConfig.MinRiskRewardRatio`** (config-driven), scalp default **1.2** (within 1.2–1.5).
3. Eligibility: **hard-exclude** coins whose 15m ATR exceeds the cap (drop from pool).
4. Config surface: **new per-strategy fields on `RiskControlConfig`** (where `MinRiskRewardRatio`/`RiskPerTradePct` already live and are code-enforced + clamped).

## Summary

A targeted, additive change across `store`, `kernel`, and `trader` — no new abstractions. ATR derivation is centralized in one exported kernel function (shared by validation-adjacent sizing, the execution fallback, and the forced-open path) to guarantee sign consistency; eligibility filtering happens *after* market data is fetched (where ATR exists), not in the static deny-list `filterExcludedCoins`.

## Current-state analysis

**Exit/RR control flow (end-to-end).**
- The AI path: `buildTradingContext` → `GetFullDecisionWithStrategy(ctx, mcp, engine, "balanced")` (`trader/auto_trader_loop.go:116`) → `fetchMarketDataWithStrategy` fills `ctx.MarketDataMap[sym]` = `market.GetWithTimeframes(sym, [15m,...], "15m", count)` → `BuildSystemPrompt`/`BuildUserPrompt` → `parseFullDecisionResponse(aiResp, equity, levs, ratios, riskPerTradePct)` (`kernel/engine_analysis.go:327`) → `validateDecisions(...)` → `validateDecision(...)`.
- `validateDecision` (`kernel/engine_position.go:88`) hardcodes the R/R floor at `riskRewardRatio < 3.0` (`:138`), using a *synthetic inferred entry* `stop + (tp−sl)×0.2` (`:132/:134`) — so with ATR stops (stop=1.5A, target=2A) the *inferred* R/R is `2.8A/0.7A = 4.0`, i.e. the floor rarely binds even when lowered (see Risks).
- `store.RiskControlConfig.MinRiskRewardRatio` (`store/strategy.go:957`, JSON `min_risk_reward_ratio`, default `3.0` at `:1052`, clamp `[1.0,10.0]` at `:25-26,130-135`) feeds **only** the prompt ("AI guided") — not `validateDecision`.
- No per-strategy absolute SL%/TP% for the AI strategy (only `GridStrategyConfig.StopLossPct` at `store/strategy.go:786`, unrelated).
- No scalp/swing mode in the config schema — only free-text `prompt_variant` → `writeModeVariant` (`kernel/engine_prompt.go:503-523`, maps `aggressive|conservative|scalping`), but the trader loop hardcodes `"balanced"` (`auto_trader_loop.go:116`), so the dormant `scalping` variant is never active.
- The execution path (both AI and forced opens) funnels through `executeOpenLongWithRecord`/`executeOpenShortWithRecord` (`trader/auto_trader_orders.go:87`/`:208`), which **bypass `validateDecisions` entirely**.

**Execution ordering (the core gap).** `executeOpen*` runs: `GetWithExchange` → `applyAutopilotFullSizeOpen` (`:155`; internal risk cap at `trader/auto_trader_risk.go:391` is **gated on `SL>0 && TP>0`**) → `enforcePositionValueRatio` → margin auto-reduce → `enforceMinPositionSize` → compute `quantity` → `ensureStopLossTakeProfitDefaults` (fills −3%/+8%, `:196-200`, fn at `:35`) → `Open*` → `attachStopLossTakeProfit` (`:57`). Forced opens (`trader/auto_trader_force.go:103-110`) construct `Decision` with SL/TP=0, so the execution risk cap is skipped and SL/TP are filled only *after* size is committed. **Everything needing the stop known *before* sizing is currently wrong.**

**Eligibility.** `filterExcludedCoins` (`kernel/engine.go:492`) is a static deny-list applied inside `GetCandidateCoins` — *before* any market data exists, so it cannot see ATR. `CandidateCoin` (`engine.go:55`) carries only `Symbol` + `Sources`. Risked candidates are pruned later after `fetchMarketDataWithStrategy` fills `ctx.MarketDataMap` (`engine_analysis.go:224-271`, positions `:250`, candidates `:271`). There is no volatility criterion anywhere; `tradableSnapshot` (`service/signal/service.go:224`) is an upstream ingest-level gate (`!IsDelisted && MarkPx>0`), unrelated to trading-symbol volatility.

**15m ATR availability.** `market.GetWithTimeframes(sym, tfs, "15m", n)` (`market/data.go:148`) exposes the 15m ATR14 **only** via `TimeframeData["15m"].ATR14` (`market/types.go:44` `TimeframeSeriesData.ATR14`, set by `calculateTimeframeSeries` at `data_klines.go:235`). The top-level `market.Data` struct has **no `ATR14` field** (`market/types.go:6-20`); its only ATRs are `IntradaySeries.ATR14` (3m/5m) and `LongerTermContext.ATR14` (4h). `GetWithExchange` (`data.go:34`) therefore carries no usable 15m ATR — **not** a valid ATR source. `EnableATR` gates ATR emission in the prompt (`engine_prompt.go:1224`, `data.ATR14`), i.e. the AI sees 15m ATR14 iff `EnableATR` is on and "15m" is a selected timeframe (`EnableATR` default is `false`, `store/strategy.go`). Signal-service `Rank`/`SignalLab`/`asset` expose **no** ATR number (`service/signal/compute.go:33`, `provider/vergex/client.go:48`, `service/signal/service.go:18`). `getKlinesFromHyperliquid` is unexported (`data_klines.go:116`); `market.ExportCalculateATR` (`data_indicators.go:218`) is exported.

**Reusable / blocking.**
- Reusable: `RiskCappedPositionSize` (`engine_position.go:20`) is price-move based, leverage-independent — just needs a real stop. `market.GetWithTimeframes` is the one-call 15m ATR14 source. `ensureStopLossTakeProfitDefaults` already has the entry-price + no-op + error contract. `attachStopLossTakeProfit` is the clean post-open seam.
- Blocking: `validateDecisions`/`validateDecision`/`parseFullDecisionResponse` take scalar params, not the config — R/R must be threaded as a new scalar. `applyAutopilotFullSizeOpen`'s risk cap requires the stop to exist *before* it runs. `filterExcludedCoins` structurally cannot enforce an ATR cap (no market data) — the real filter must run post-fetch.

**Real volatility (Hyperliquid 15m, last ~100 bars).** KAITO avg 15m range **0.97%** (max 4.6%) · HEMI **4.53%** (max 15.3%) · ACE **5.94%** (max 31.3%). Fixed −3%/+8% fails both ends: on low-vol coins +8% is unreachable (→ long holds); on high-vol coins −3% sits *inside* normal 15m noise (→ frequent stop-outs). R/R≥3 with ATR-scaled stops makes targets even more unreachable on high-vol coins (HEMI target ≈ 3×1.5×ATR ≈ 20%).

**Prior art.** An earlier plan proposed fixed 1×ATR stop / 3×stop target (R/R=3) — rejected because `target=3×stop` is unreachable for sub-day high-vol scalps; this plan replaces it with 1.5×ATR/2×ATR. Deployed risk features on this branch: stop-risk sizing `RiskPerTradePct=3` (`c40f447`), drawdown size-trim, correlation block `0.7` (`13abf039`).

## Design

### 1. `RiskControlConfig` — new scalp fields (`store/strategy.go`)

Add four fields plus clamp constants:

```go
// in RiskControlConfig (:957 area)
ATRStopMultiplier       float64 `json:"atr_stop_multiplier"`        // stop = entry ∓ this×ATR14
ATRTargetMultiplier     float64 `json:"atr_target_multiplier"`      // target = entry ± this×ATR14
ATREligibilityCapPct    float64 `json:"atr_eligibility_cap_pct"`    // 0 = disabled; else hard-exclude coins with ATR%/price > cap
ATREligibilityTimeframe string  `json:"atr_eligibility_timeframe"`  // default "15m"
```

- **Consts:** `MinATRStopMultiplier=0.5, MaxATRStopMultiplier=10.0, MinATRTargetMultiplier=0.5, MaxATRTargetMultiplier=10.0, MinATREligibilityCapPct=0.5, MaxATREligibilityCapPct=10.0`.
- **ClampLimits (`:44+`):** when a multiplier `>0`, clamp to `[min,max]` (0 = "use default"). When `ATREligibilityCapPct > 0`, clamp to `[min,max]` and normalize `ATREligibilityTimeframe` via `normalizeTimeframe`, defaulting to `"15m"` if empty. Follow the `CorrelationBlockThreshold` convention: `<=0` disables the cap (meaningful opt-out); multiplier `0` is nonsense, treated as "unset → default".
- **GetDefaultStrategyConfig (`:1007`):** `MinRiskRewardRatio` **`3.0 → 1.2`** (`:1052`); `Indicators.EnableATR` **`false → true`**; new fields `ATRStopMultiplier: 1.5`, `ATRTargetMultiplier: 2.0`, `ATREligibilityCapPct: 3.0`, `ATREligibilityTimeframe: "15m"`. (`PrimaryTimeframe`/`SelectedTimeframes` already `"15m"`.)
- **StrategyClampWarnings:** add entries for the capped new fields (cap + multipliers) for UI parity.

Why default R/R = **1.2** (within the user's 1.2–1.5 range): real-entry R/R = 1.33 must pass, and 1.2 leaves headroom while still rejecting degenerate `<1.2`. The hard floor `MinRiskReward=1.0` stays untouched (`:25`).

### 2. ATR stop/target derivation — one shared source (`kernel/engine_position.go`)

Centralize the arithmetic so long/short sign conventions are identical across validation-adjacent sizing, `auto_trader_orders.go`, and the forced-open path:

```go
// ATRStopTarget returns the stop/target price for the given side using
// stop = entry ∓ stopMult*atr14, target = entry ± targetMult*atr14.
// ok=false when atr14<=0 or either derived leg would be <=0 (caller falls back to fixed %).
func ATRStopTarget(entryPrice, atr14, stopMult, targetMult float64, isLong bool) (stop, target float64, ok bool)
```

- Long: `stop = entry − stopMult·atr`, `target = entry + targetMult·atr`.
- Short: `stop = entry + stopMult·atr`, `target = entry − targetMult·atr` (mirrors existing short signs: `SL > TP` for shorts, matching `validateDecision`'s ordering check at `:121/:125`).
- **Validity** (degenerate low-price coins, e.g. HEMI) is **side-aware** — the at-risk leg must stay on the correct side of 0:
  - Long: invalid if `atr14 <= 0 || entry − stopMult·atr14 <= 0` (stop below entry must stay > 0; the target `entry + targetMult·atr` is auto-valid).
  - Short: invalid if `atr14 <= 0 || entry − targetMult·atr14 <= 0` (target below entry must stay > 0; the stop `entry + stopMult·atr` is auto-valid).
  Placed next to `RiskCappedPositionSize` (it feeds it) — pure math, no state, trivially unit-testable; the unit test must cover the short-side degenerate case explicitly.

**`validateDecision` R/R wiring.** Thread the scalar:

```go
func validateDecisions(decisions []Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int,
    btcEthPosRatio, altcoinPosRatio, riskPerTradePct, minRiskRewardRatio float64) error
// validateDecision gains the same trailing param
```

Replace `if riskRewardRatio < 3.0 {` (`:138`) with a **defensively floored** comparison and updated message:

```go
floor := minRiskRewardRatio
if floor < 1.0 { floor = 1.0 } // hard floor, mirrors store.MinRiskReward
if riskRewardRatio < floor {
    return fmt.Errorf("risk/reward ratio too low (%.2f:1), must be ≥%.1f:1 [risk: %.2f%% reward: %.2f%%] ...", riskRewardRatio, floor, ...)
}
```

**`parseFullDecisionResponse` threading** (`engine_analysis.go:327`): add `minRiskRewardRatio float64` parameter, forward to `validateDecisions`. `GetFullDecisionWithStrategy` passes `riskConfig.MinRiskRewardRatio`. No other caller exists (verify via `grep parseFullDecisionResponse`); tests updated in lockstep.

**Known behavior to document in a comment (not a bug):** `validateDecision`'s R/R is measured on the synthetic 20%-of-span entry, so ATR scalps report ~4.0 there even though real-entry R/R is 1.33. The config floor therefore mostly governs *AI-supplied arbitrary stop/target pairs* reaching `validateDecision`, not the formulaic ATR ones. Do **not** change the inferred-entry heuristic — it is reused in three places and tied to replay tuning; the task only makes the floor config-driven.

### 3. Eligibility cap — post-fetch kernel filter (`kernel/engine_analysis.go`)

`filterExcludedCoins` stays **unchanged** (pre-fetch, no ATR). Add a post-fetch pass that runs right after the existing candidate prune in `GetFullDecisionWithStrategy`:

```go
// Filter candidates whose ATR%/price on the eligibility timeframe exceeds
// RiskControl.ATREligibilityCapPct. Applied only to the 15m scalp pool:
// skipped when cap<=0 (disabled) or PrimaryTimeframe != "15m".
// Never filters held positions (they were eligible at entry).
func filterCandidatesByATRCap(ctx *Context, engine *StrategyEngine)
```

- Read cap / tf from `engine.GetConfig().RiskControl` (`ATREligibilityCapPct`, `ATREligibilityTimeframe`).
- **Gate:** `if cap <= 0 || strings.TrimSpace(tf)=="" || engine.GetConfig().Indicators.Klines.PrimaryTimeframe != "15m" { return }` — scopes the hard-exclude to the 15m scalp bot and makes the cap a per-strategy opt-in for legacy configs.
- For each `ctx.CandidateCoins[i]`: `d := ctx.MarketDataMap[sym]`; compute `atrPct = d.TimeframeData[tf].ATR14 / d.CurrentPrice * 100` (denominator is the fetched `Data.CurrentPrice`, the primary-TF close == 15m close here). If `atrPct > cap` → log (`"🚫 Excluded coin %s: 15m ATR %.2f%% > cap %.2f%%"`) and drop from `ctx.CandidateCoins`. **No top-level `Data.ATR14` fallback** — the field does not exist (`market/types.go:6-20`); read only `TimeframeData[tf].ATR14`.
- **Eligibility is `fail-open` (deliberate):** when ATR is missing or `atrPct` can't be computed (data absent / `ATR14<=0` / nil `TimeframeData[tf]`), the candidate is **admitted** with a log line (`"⚠️ ATR cap skipped for %s: no 15m ATR"`), so a transient fetch gap never silently empties the pool. Tradeoff (documented): a high-ATR coin whose 15m ATR transiently fails passes eligibility *and* falls back to fixed −3%/+8% stops — the pre-change behavior for the riskiest coins. Operator-visible admit logging is required so this is observable, not silent.
- **Timeframe guard:** the cap is effectively disabled (all admitted) when `tf` is not among `SelectedTimeframes`/`PrimaryTimeframe` (i.e. `d.TimeframeData[tf]` would be nil). `ClampLimits` normalizes the string but must be joined by this runtime guard so an operator-set `ATREligibilityTimeframe` not in the selected set does not silently fail-open every check.
- Positions already held are **not** filtered.

**Forced-open path** — `ensureLongShortCoverage` (`auto_trader_force.go:33`) draws from `DirectionalCandidates` (the vergex ranking cache), *not* `ctx.CandidateCoins`, so the kernel filter does not cover it. Add the same check inside `fill()` before constructing the forced `Decision`, reading `ctx.MarketDataMap` (already populated by the time runCycle executes decisions — **no extra network call**):

```go
if cfg.RiskControl.ATREligibilityCapPct > 0 &&
    engine.GetConfig().Indicators.Klines.PrimaryTimeframe == "15m" {
    if d, ok := ctx.MarketDataMap[c.Symbol]; ok {
        if atrPct(d, tf) > cfg.RiskControl.ATREligibilityCapPct {
            at.logInfof("⚖️ Skipped forced %s %s: 15m ATR %.2f%% exceeds cap", action, c.Symbol, atrPct)
            continue
        }
    }
}
```

The forced pool draws from `engine.DirectionalCandidates()` (vergex ranking, `auto_trader_force.go:59`) — a **separate universe** from `ctx.CandidateCoins`, so a forced candidate may be **absent from `MarketDataMap`** (which is populated only for positions + AI candidates). To keep forced-eligibility consistent with the AI pool: read `ctx.MarketDataMap[c.Symbol]` if present; if **absent and cap active**, call `fetchATR14(c.Symbol)` (the §4 helper, one `GetWithTimeframes` call) to compute `atrPct` on demand. Fail-open only when ATR is still unobtainable. Forced opens are a small top-up (≤2/cycle), so the on-demand fetch is bounded. No `CandidateCoin` struct change — eligibility is enforced functionally on post-fetch data.

### 4. Execution-time ordering fix (`trader/auto_trader_orders.go`, `auto_trader_risk.go`)

**Reorder `executeOpen*` so the stop exists before sizing.** The ATR fill currently happens post-sizing (`:196-200`); move it to *pre-sizing*, right after `market.GetWithExchange` provides `CurrentPrice` and before `applyAutopilotFullSizeOpen`. Because `applyAutopilotFullSizeOpen` is invoked *again* inside `executeOpen*` (it re-sizes each open — the force-time call in `force.go` is overwritten here), a pre-filled stop makes its internal risk cap (`auto_trader_risk.go:391`) **run with a real stop** for forced opens. That is the entire fix; no change to the `SL>0 && TP>0` gate itself (it still correctly no-ops the force-time call where no stop exists yet).

**`ensureStopLossTakeProfitDefaults` — ATR-aware (new signature):**

```go
func (at *AutoTrader) ensureStopLossTakeProfitDefaults(d *kernel.Decision, entryPrice, atr14 float64) error
```

- Both `>0` → **no-op** (never override a full pair; AI-sourced stops stay authoritative).
- `entryPrice <= 0` → error (clean pre-open abort).
- `atr14 > 0` and `kernel.ATRStopTarget(entryPrice, atr14, at.atrStopMultiplier(), at.atrTargetMultiplier(), isLong)` returns `ok` → fill ATR levels (sign by `d.Action`).
- Else → fall back to fixed `defaultStopLossPct`/`defaultTakeProfitPct` (−3%/+8%) — the existing `fail-to-fixed` behavior for missing ATR / new listings / degenerate low-price coins.
- Log which path was taken (`"Filled ATR default SL/TP ..."` vs `"Filled fixed default SL/TP ..."`).

**New pre-sizing block** (insert in both `executeOpenLongWithRecord` and `executeOpenShortWithRecord`, replacing the current post-sizing `ensureStopLossTakeProfitDefaults` + `actionRecord.StopLoss=...` block):

```go
if decision.StopLoss <= 0 || decision.TakeProfit <= 0 {
    atr14 := at.fetchATR14(decision.Symbol)               // 0 on failure/absent → fixed fallback
    if err := at.ensureStopLossTakeProfitDefaults(decision, marketData.CurrentPrice, atr14); err != nil {
        return fmt.Errorf("aborting %s open without SL/TP: %w", decision.Symbol, err)
    }
    actionRecord.StopLoss = decision.StopLoss
    actionRecord.TakeProfit = decision.TakeProfit
}
// ...then existing applyAutopilotFullSizeOpen → enforcePositionValueRatio → margin → min-size → quantity
```

The `actionRecord` update moves here (persisting the *effective* filled values into `runCycle`'s saved record, which reads `actionRecord` after execution — no `auto_trader_loop.go` change). `attachStopLossTakeProfit` post-open is unchanged and now always sees `>0` prices.

**Helpers (in `auto_trader_orders.go`, read config with 0→default):**

```go
func (at *AutoTrader) atrStopMultiplier() float64   // config.AtrStopMultiplier, default 1.5
func (at *AutoTrader) atrTargetMultiplier() float64 // config.AtrTargetMultiplier, default 2.0
func (at *AutoTrader) atrTimeframe() string          // config.AtrEligibilityTimeframe, default "15m"
func (at *AutoTrader) fetchATR14(symbol string) float64 // market.GetWithTimeframes(sym, []string{tf}, tf, 30) -> d.TimeframeData[tf].ATR14; 0 on error/absent (NOT top-level Data.ATR14 — that field does not exist)
```

`fetchATR14` reuses `GetWithTimeframes` (the one-call 15m ATR14 source, consistent with the kernel's data source for the same coin). Per-open cost is one extra call but only when a decision omits SL/TP (forced opens; AI opens with both set skip it).

**`auto_trader_risk.go`:** no code change; add a comment that the `SL>0 && TP>0` gate now *does* run for forced opens at execution because `executeOpen*` fills ATR stops pre-sizing.

### 5. Prompt + ATR visibility (`kernel/engine_prompt.go`, `store/strategy.go`)

**`vergexHoldRules()` (`:263-268`):** replace the `−3%/+8%` string literals with ATR-scaled guidance and the new R/R:
- Old: `"stop-loss around -3% and take-profit around +8% or beyond"`, `"...around -3%... (around +8%)"`.
- New: `"set stop-loss at 1.5× the coin's 15m ATR14 and take-profit at 2× the stop (R/R ≈ 1.33)"` — always ATR-scaled per the coin's own 15m ATR14 shown in its market data; remove the `+8%` target example and the fixed-percent framing. Keep fee/noise/churn guidance but make any remaining magnitude references ATR-relative (e.g. "targets well beyond fees" instead of "+8%"). Keep the first/last bullets referencing hold/throttle behavior intact; note in a comment that the numbers mirror `auto_trader_throttle.go` constants (intentionally left untouched — see Risks).

**ATR visibility:** `EnableATR` default → `true` and `SelectedTimeframes` already `["15m"]`, so `formatTimeframeSeriesData` emits `ATR14: <value>` for the 15m block (`:1224`) — no prompt-code change beyond the text. This flips *default* configs; existing persisted strategies keep their saved `enable_atr`/timeframes.

**`writeModeVariant` / loop variant:** **no change.** `auto_trader_loop.go` stays `"balanced"` (`:116`); the scalp outcome is produced purely by config (ATR multipliers, R/R floor, eligibility cap). Activating the dormant `scalping` variant is a separate concern and out of scope — keeps the loop behavior stable.

### 6. Concurrency & error handling

- **Concurrency:** all new code runs in the existing single `runCycle` goroutine (sequential per-decision loop). No new goroutines, locks, or shared mutable state. `fetchATR14` is a synchronous blocking network call consistent with existing per-decision I/O.
- **Errors:** `ensureStopLossTakeProfitDefaults` pre-fill error → pre-open abort (no position). `attachStopLossTakeProfit` failure stays fatal-error-on-record (unchanged). ATR fetch failure / ATR=0 / degenerate low price → silent `fail-to-fixed` (−3%/+8%), never an abort. ATR eligibility always `fail-open`.
- **Ordering guarantees:** pre-filled ATR stops make `applyAutopilotFullSizeOpen`'s cap authoritative for forced opens; `enforcePositionValueRatio` + `enforceMinPositionSize` still run after it; nothing downstream re-widens the stop.

## File-by-file impact

| File | Change | Why | Deps |
|---|---|---|---|
| `store/strategy.go` | **Modify.** 4 consts + 4 `RiskControlConfig` fields; extend `ClampLimits` + `StrategyClampWarnings`; defaults (`MinRiskRewardRatio 3.0→1.2`, `EnableATR false→true`, multipliers, cap, timeframe). | New per-strategy scalp surface + defaults; additive JSON (no migration). | none |
| `api/handler_user.go` | **Modify.** `:275` hardcodes `MinRiskRewardRatio = 3.0` on the default-strategy create path (the live 15m bot) — lower to **1.2**. | Without this, the `GetDefaultStrategyConfig` 1.2 default is overwritten and the config-driven R/R floor is dead on the live path. | none |
| `kernel/engine_position.go` | **Modify.** Add exported `ATRStopTarget`; thread `minRiskRewardRatio` through `validateDecision`/`validateDecisions`; replace hardcoded `3.0` with floored `floor`. | Source of truth for ATR math + config-driven R/R. | store consts (1.0 floor) |
| `kernel/engine_analysis.go` | **Modify.** `parseFullDecisionResponse` + `GetFullDecisionWithStrategy` thread `riskConfig.MinRiskRewardRatio`; add `filterCandidatesByATRCap` + call after the candidate prune. | Eligibility at the point where ATR exists; R/R wiring. | §2, §3 |
| `kernel/engine_prompt.go` | **Modify.** `vergexHoldRules()` ATR-scaled text (remove −3%/+8% literals). | Prompt ↔ code sync. | none |
| `trader/auto_trader_orders.go` | **Modify.** Reorder `executeOpen*` (ATR pre-fill before sizing + `actionRecord` update); `ensureStopLossTakeProfitDefaults` gains `atr14`; add `fetchATR14`/multiplier/timeframe helpers. | Ordering fix + ATR fallback; forced opens get capped size. | §2, §4 |
| `trader/auto_trader_risk.go` | **Modify.** Comment-only on the `SL>0&&TP>0` gate. | Documents why the cap now runs. | §4 |
| `trader/auto_trader_force.go` | **Modify.** In `fill()`, skip candidates whose `ctx.MarketDataMap` ATR% exceeds cap (15m + cap active). | Hard-exclude high-ATR coins from the forced pool too (it bypasses the kernel pool). | §3, `ctx.MarketDataMap` |
| `trader/auto_trader_loop.go` | **No change** (variant stays `"balanced"`). | Scalp via config, not variant. | — |
| `trader/auto_trader_throttle.go` | **No change.** | Fixed % bands intentionally kept (Risks). | — |
| `kernel/engine.go` (`filterExcludedCoins`), `market/*`, `service/signal`, `provider/*` | **No change.** | `filterExcludedCoins` can't see ATR; `GetWithTimeframes`/`ExportCalculateATR` already exported. | — |
| tests (new/modified) | `store` default+clamps; `kernel` `ATRStopTarget` signs, R/R floor, `filterCandidatesByATRCap`; `trader` `ensureStopLossTakeProfitDefaults` ATR/fixed/no-op/degenerate, forced-eligibility skip. | Prove each seam without network. | each step |

## Execution index

| # | Work item | Goal | Done when | Key files | Dependencies | Size |
|---|---|---|---|---|---|---|
| 1 | Config surface | New scalp fields + defaults, R/R default lowered | 4 fields clamped+defaulted; `MinRiskRewardRatio:1.2`, `EnableATR:true` | `store/strategy.go` | none | S |
| 2 | ATR math + R/R threading | Config-driven R/R floor + shared ATR arithmetic | `ATRStopTarget` added; no hardcoded `3.0`; floor ≥1.0 | `kernel/engine_position.go`, `kernel/engine_analysis.go` | #1 | M (atomic w/ tests) |
| 3 | Eligibility filter | Hard-exclude high-ATR coins from pool | `filterCandidatesByATRCap` runs post-fetch; positions exempt; forced opens skipped | `kernel/engine_analysis.go`, `trader/auto_trader_force.go` | #1, #2 | M |
| 4 | Prompt | ATR-scaled stop/target guidance | No −3%/+8% literals; states R/R≈1.33 | `kernel/engine_prompt.go` | #1 | S |
| 5 | Execution ordering | Forced opens get ATR stop before sizing | Pre-sizing ATR fill; risk cap runs on real stop | `trader/auto_trader_orders.go`, `auto_trader_risk.go` | #2 | M |
| 6 | Tests | Prove each seam without network | All unit tests listed in Verification pass | `*_test.go` across store/kernel/trader | #1–#5 | M |

## Implementation order

Each step compiles/testable independently unless noted.

1. **Config surface (`store/strategy.go`)** — 4 consts, 4 fields, clamp + warnings, defaults (`MinRiskRewardRatio:1.2`, `EnableATR:true`, multipliers, cap, timeframe).
   **Also `api/handler_user.go:275`** — lower the hardcoded `MinRiskRewardRatio = 3.0` to `1.2` (the live create path that would otherwise overwrite the new default).
2. **ATR math + R/R threading (`kernel/engine_position.go`, `kernel/engine_analysis.go`)** — add `ATRStopTarget`; thread `minRiskRewardRatio` through `validateDecision`/`validateDecisions`/`parseFullDecisionResponse`/`GetFullDecisionWithStrategy`, replacing the `3.0`. *Atomic with test-call-site updates.*
3. **Eligibility filter (`kernel/engine_analysis.go`)** — `filterCandidatesByATRCap` + call in `GetFullDecisionWithStrategy` (15m gate, fail-open, positions exempt).
4. **Prompt (`kernel/engine_prompt.go`)** — `vergexHoldRules()` ATR-scaled text.
5. **Execution ordering (`trader/auto_trader_orders.go`, `auto_trader_risk.go`, `auto_trader_force.go`)** — reorder `executeOpen*` with pre-sizing ATR fill; ATR-aware `ensureStopLossTakeProfitDefaults`; config-read helpers + `fetchATR14`; forced-open eligibility skip via `ctx.MarketDataMap`. *This step lands as one unit (pre-fill + cap + forced-skip together).*
6. **Tests** for each seam; `go build ./...` and `go test ./store/... ./kernel/... ./trader/...`; `gofmt`.
7. **Manual/replay gate (before trusting the cap default):** replay the 15m history through the eligible pool to confirm the 3% cap yields a sane tradable universe under `GetWithTimeframes`'s actual feed, and observe exchange SL/TP ≈ entry∓1.5×ATR / entry±2×ATR.

## Verification

- `grep -rn "3.0" kernel/engine_position.go` → no hardcoded R/R floor (only the config read + floored `1.0`).
- RiskControlConfig has new scalp fields (stop/target multiples, ATR cap + timeframe) with clamps in `ClampLimits` and defaults in `GetDefaultStrategyConfig`.
- High-ATR candidates (15m ATR14 > cap) filtered at the post-fetch kernel layer **and** forced opens; held positions never filtered.
- Forced opens get ATR stop/target **before** `applyAutopilotFullSizeOpen` so `RiskCappedPositionSize` runs on a real stop.
- `vergexHoldRules` no longer mentions −3%/+8%; states ATR-scaled stop/target R/R≈1.33; `EnableATR` ON with "15m" timeframe.
- Unit tests: `ATRStopTarget` long/short signs + degenerate (incl. short-side); `validateDecision` R/R floor (config-driven, hard 1.0); `filterCandidatesByATRCap` (fail-open, 15m gate, positions exempt, admit-logging); `ensureStopLossTakeProfitDefaults` (ATR vs fixed vs no-op vs degenerate); forced-eligibility skip. Note: `fetchATR14` itself is not unit-testable (direct `GetWithTimeframes` call) — covered by the manual/replay gate; only `ensureStopLossTakeProfitDefaults` (with a passed-in `atr14`) is unit-tested. The forced-eligibility test must use a `Context` with a **populated** `MarketDataMap` + fake `DirectionalCandidates` (the existing empty-map helpers exercise only the fail-open branch).
- `go build ./...`; `go test ./store/... ./kernel/... ./trader/...`; `gofmt`.

## Risks & migration

- **No schema migration** — all new JSON fields are additive; legacy configs read them as `0`. `0` maps to: multipliers → defaults (via helpers); `ATREligibilityCapPct` → **disabled** (opt-in), so a persisted legacy strategy gets no eligibility filtering until the field is set. Deliberate (matches `CorrelationBlockThreshold`'s `<=0` disables), non-breaking. Rollback = revert; extra JSON fields ignored by older builds.
- **Effective R/R floor per strategy = its persisted `min_risk_reward_ratio` or default 1.2.** Because validation measures on the implied entry (R/R≈4.0 for ATR scalps), a legacy 3.0 value does *not* block ATR scalps — the config floor mostly binds AI-arbitrary stop pairs. Pre-existing quirk, left as-is.
- **Create-path dependency:** `api/handler_user.go:275` hardcodes `MinRiskRewardRatio = 3.0`. If the live 15m bot's strategy is created via that handler (it is — it builds the default "NOFX Claw402 Auto Strategy"), the lowered 1.2 default in `GetDefaultStrategyConfig` is overwritten unless `:275` is updated in the same step. Verify which create path the live bot uses before trusting the default.
- **`EnableATR` is a persisted field.** Flipping the default only affects new configs; an existing bot with `enable_atr:false` won't show ATR14 while its prompt tells it to use ATR14. Mitigate at deploy: confirm the live strategy's `enable_atr`/`selected_timeframes` include 15m, or force `EnableATR=true` for 15m strategies in `ClampLimits` (global but single-bot today).
- **ATR data source:** `GetWithTimeframes` uses Hyperliquid for xyz assets and **CoinAnk/Binance** for crypto alts. The kernel AI path and the new execution fallback share this source (consistent), but it may differ from the documented Hyperliquid-15m volatility numbers (KAITO 0.97% / HEMI 4.53% / ACE 5.94%). A coin eligible on Hyperliquid but ineligible on Binance data could be excluded (or vice-versa). Validate the cap default (3%) against the *actual live feed* used by `GetWithTimeframes`; tune `ATREligibilityCapPct` if CoinAnk's 15m ATR% runs hot/cold.
- **Throttle constants are ATR-inconsistent.** `earlyCloseStopLossBypassPct=-3.0`/`earlyCloseTakeProfitBypassPct=8.0`, `noiseCloseLossFloorPct=-2.0`/`noiseCloseProfitCeilingPct=3.0`, and `drawdownClosePriceGainPct=5.0` remain fixed-% while stops are now ATR-scaled and targets can be `+3%`-ish. Left **unchanged** deliberately (deploy-test the scalp first); follow-up is a data-gated retune of these constants to ATR-relative values. Flag for the operator, don't expand this change.
- **Test-suite updates are mandatory and atomic with the signature changes** — every `validateDecisions`/`validateDecision`/`parseFullDecisionResponse` call site in `kernel` tests must add the `minRiskRewardRatio` argument; a compile break is expected and intended.

## Open questions (resolved + remaining)

Resolved: R/R wired to config (default 1.2); stop=1.5×ATR / target=2×ATR; hard-exclude high-ATR; config on `RiskControlConfig`; `api/handler_user.go:275` also updated; ATR read from `TimeframeData[tf].ATR14` (not a top-level field); forced-pool eligibility uses on-demand `fetchATR14` for symbols absent from `MarketDataMap`; eligibility is **fail-open** with admit logging.

Remaining (material):
1. **Exact 15m-ATR eligibility cap value** — default 3% is a placeholder; data-gate against the live pool's ATR distribution under `GetWithTimeframes`'s actual feed (CoinAnk vs Hyperliquid) before trusting it.
2. **`EnableATR` global flip** — default ON; confirm the live strategy's persisted `enable_atr`/timeframes actually expose 15m ATR14 (else force ON in `ClampLimits` for 15m strategies).

## References

- `store/strategy.go:25-26,44,130-135,786,905-916,947-958,1007,1052`
- `kernel/engine_position.go:20,88,121-138,158`
- `kernel/engine_prompt.go:263-268,503-523,1145-1160,1224`
- `kernel/engine.go:55,277,492`, `kernel/engine_analysis.go:224-271,327`
- `market/data.go:34,148,601`, `market/data_klines.go:116,235`, `market/data_indicators.go:86,218`, `market/types.go:6-20,44`
- `trader/auto_trader_orders.go:27-35,87,155,196-200,208`
- `trader/auto_trader_force.go:33,103-120`
- `trader/auto_trader_risk.go:391-423`, `trader/auto_trader_loop.go:116,261,595`
- `service/signal/service.go:224`, `service/signal/compute.go:33`, `provider/vergex/client.go:48`, `api/handler_user.go:275`
- Related: `docs/plans/fix-unprotected-forced-opens-2026-08-14.md`
