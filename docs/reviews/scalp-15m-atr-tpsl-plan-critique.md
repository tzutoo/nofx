# Critique — Scalp 15m ATR TP/SL Plan (`scalp-15m-atr-tpsl-2026-08-16.md`)

Reviewer: opencode · Date: 2026-08-17
Baseline plan: `docs/plans/scalp-15m-atr-tpsl-2026-08-16.md`
Baseline export (only its generated plan counts as plan content): `prompt-exports/oracle-plan-2026-08-17-095650-atr-adaptive-15m-sca-0850.md`

This critique covers only (1) implementation-bearing content the export carries that the plan lost, (2) under-specified seams / unresolved material decisions / incorrect references / missing dependencies, (3) details the code disproves or a simpler design replaces (with the precise correction), (4) requirements/edge cases absent from both, and (5) questions whose answers would change the design. Code was read directly; line refs below are from the actual files.

---

## Context / Scope

The plan is largely sound: the execution-ordering diagnosis is correct and the R/R threading is minimal. The critique is dominated by **one structural error in the ATR data-source assumption** (spot-check c) that propagates into two compile-breaking design seams, plus a set of smaller seams around the forced-open eligibility path and one missing config-write dependency.

---

## Spot-check results (as requested)

| # | Question | Verdict |
|---|---|---|
| (a) | `ensureLongShortCoverage`'s `fill()` access to `ctx.MarketDataMap`; exact candidate-prune fn name | ✅ `fill()` is a closure over `ctx` (`trader/auto_trader_force.go:63`), so `ctx.MarketDataMap` is in scope with no signature change. Prune fn = `pruneCandidateCoinsWithoutMarketData(ctx *Context)` (`kernel/engine_analysis.go:262`). **But** see §2.3 for a correctness gap in how it is populated for forced candidates. |
| (b) | `MinRiskRewardRatio` read sites; exact `validateDecisions`/`parseFullDecisionResponse` signatures | ✅ Prompt-only reads (`kernel/engine_prompt.go:342/360/589/592`) + clamp (`store/strategy.go:130-134`) + default write (`store/strategy.go:1053`, `api/handler_user.go:275`). Signatures confirmed. **But** the plan/export miss the `api/handler_user.go:275` hardcoded `3.0` default — see §2.4. |
| (c) | Does `GetWithTimeframes([]string{"15m"}, "15m")` return usable `Data.ATR14`? | ❌ **No.** Top-level `market.Data` has **no `ATR14` field at all** (`market/types.go:6-20`). ATR14 lives only in `TimeframeData[tf].ATR14` (set by `calculateTimeframeSeries` → `data.ATR14 = calculateATR(klines,14)`, `market/data_klines.go:235`). Both plan and export claim `Data.ATR14` exists — see §1.1, §2.1. |
| (d) | `EnableATR` default in `GetDefaultStrategyConfig` | ✅ `false` (`store/strategy.go:1018`). Accurate. |

---

## 1. Implementation-bearing content from the export that the plan loses

### 1.1 The export names a top-level `Data.ATR14` that does not exist — and the plan inherited it

This is the single largest defect, and it is shared by both documents, so it is reported here as a shared root cause whose fixes land in the plan.

- **Export** (`architecture` and generated-plan "15m ATR availability"): *"`GetWithTimeframes(...)` returns top-level `Data.ATR14` = primary-TF ATR14 (15m when primary is "15m") and `TimeframeData["15m"].ATR14` also set."*
- **Plan** (§2 "15m ATR availability"): *"returns top-level `Data.ATR14` = primary-TF ATR14 and `TimeframeData["15m"].ATR14`."*

**Code disproves the first half.** `market.Data` (`market/types.go:6-20`) has fields `Symbol, CurrentPrice, PriceChange1h, PriceChange4h, CurrentEMA20, CurrentMACD, CurrentRSI7, OpenInterest, FundingRate, IntradaySeries, LongerTermContext, TimeframeData` — **no `ATR14`**. `GetWithTimeframes` returns `&Data{... TimeframeData: timeframeData}` and never sets any top-level ATR (`market/data.go:250-266`). The only top-level ATRs anywhere are `IntradaySeries.ATR14` (3m/5m, set by `GetWithExchange`) and `LongerTermContext.ATR14` (4h) — both of which the plan itself correctly rejects as invalid 15m sources.

**Correct statement:** `GetWithTimeframes` exposes the 15m ATR14 **only** via `data.TimeframeData["15m"].ATR14`. The top-level `Data.ATR14` in both docs is a phantom field.

**Consequences that must be fixed in the plan (§1, §2):**
1. `filterCandidatesByATRCap`'s proposed fallback `d.ATR14` (`kernel/engine_analysis.go` seam, plan §3.3) is a **compile error** (no such field). Remove the fallback; the primary `d.TimeframeData[tf].ATR14` path is sufficient and correct.
2. `fetchATR14` helper (plan §3.4) spec `GetWithTimeframes(sym, []string{tf}, tf, 30) -> Data.ATR14` **cannot compile and would otherwise silently return 0**. It must read `data.TimeframeData[tf].ATR14` (the `TimeframeData` entry for the eligibility timeframe), not a top-level field.
3. The `atrPct`/`atrPct(d, tf)` helper referenced in §3.3 and the forced-open snippet is never defined in either doc and must be pinned to read `d.TimeframeData[tf].ATR14`.

Everything else about the data source holds: `GetWithExchange` does not set a 15m ATR; `getKlinesFromHyperliquid` is unexported; `ExportCalculateATR` is exported. Only the top-level-field claim is wrong.

## 2. Under-specified seams, unresolved material decisions, contradictions, incorrect references

### 2.1 `ATRStopTarget` validity expression is long-only, contradicting the short-side sign spec

The plan §3.2 spec: `ok=false if atr14 <= 0 || entry − stopMult·atr14 <= 0 || entry − targetMult·atr14 <= 0 (both legs on the correct side of 0)` — while simultaneously stating short legs are `stop = entry + stopMult·atr`, `target = entry − targetMult·atr`.

For a **short**, `stop = entry + stopMult·atr` is always > 0 when `entry > 0`, and the only degenerate leg is `target = entry − targetMult·atr ≤ 0`. The plan's formula checks `entry − stopMult·atr14 ≤ 0`, which is a *long-side* check and is wrong (and irrelevant) for shorts. The two-sided "both legs on the correct side of 0" phrasing hides this. The validity predicate must be defined **per side**:
- Long: invalid if `entry − stopMult·atr ≤ 0` (stop below entry must stay > 0) — the target `entry + targetMult·atr` is auto-valid.
- Short: invalid if `entry − targetMult·atr ≤ 0` (target below entry must stay > 0) — the stop is auto-valid.

This is exactly the kind of sign bug the plan tries to centralize away; as written it would let a degenerate short leg through and emit a negative take-profit. The unit-test list ("ATRStopTarget signs + degenerate") should include the short-side degenerate case explicitly.

### 2.2 Forced-open eligibility is only partially enforced — "consistent pools" claim overstates

Plan §3.3: *"The eligibility ATR computes from the same `ctx.MarketDataMap` values the kernel filter used, so AI pool and forced pool are consistent."* This is not guaranteed.

`ctx.MarketDataMap` is populated only for `ctx.Positions` + `ctx.CandidateCoins` (`fetchMarketDataWithStrategy`, `kernel/engine_analysis.go`). The forced-open path draws from `engine.DirectionalCandidates()` (vergex ranking, `auto_trader_force.go:59`), a **separate universe**. A `DirectionalCandidate` whose symbol is neither a position nor a candidate in the AI pool is **absent from `MarketDataMap`**, and the plan's forced-open check is explicitly fail-open for absent symbols (`"if d, ok := ctx.MarketDataMap[c.Symbol]; ok { ... }"`).

Net effect: high-ATR coins that appear only in the vergex ranking and not in the AI candidate pool are force-opened **unfiltered**. Consistency holds only for the overlapping subset. The plan should either (a) accept this and state it as a documented residual (with the 15m-gate note that forced opens are a small top-up), or (b) fetch ATR on demand for forced candidates (one `GetWithTimeframes` call, same cost as `fetchATR14`). As written, the "consistency" justification for reusing `ctx.MarketDataMap` rather than `fetchATR14` does not stand for the non-overlapping universe.

### 2.3 Forced-open eligibility TF mismatch — `fetchATR14` and the kernel filter read different timeframes

The forced-open snippet (plan §3.3) reads `ctx.MarketDataMap[c.Symbol]` at `TimeframeData[tf]` where `tf` comes from `at.atrTimeframe()` (= `ATREligibilityTimeframe`, default "15m"). The kernel `filterCandidatesByATRCap` reads the same eligibility timeframe. That part is consistent. But the plan's `fetchATR14` (§3.4) is specified to use `[]string{tf}, tf` — i.e. the eligibility timeframe as primary. Since the kernel filter and forced-open check and `fetchATR14` all agree on `ATREligibilityTimeframe`, this is consistent **as long as the eligibility timeframe is always present in `TimeframeData`**. It is only guaranteed present when it is a `SelectedTimeframe`/primary. If an operator sets `ATREligibilityTimeframe` to a value not in `SelectedTimeframes` (e.g. "1h"), `d.TimeframeData["1h"]` will be nil and every check silently fail-opens. The plan clamps/normalizes the timeframe string but never verifies it against `SelectedTimeframes`. Add a guard: cap is effectively disabled when the eligibility timeframe is not among the strategy's selected timeframes (or auto-restrict it to the primary timeframe).

### 2.4 Missing dependency: `api/handler_user.go:275` hardcodes `MinRiskRewardRatio = 3.0`

Plan/export say `MinRiskRewardRatio` feeds "only the prompt". Reads are indeed prompt-only, but there is a **write** the plan misses: `api/handler_user.go:275` sets `c.RiskControl.MinRiskRewardRatio = 3.0` when creating a default user strategy. If any create flow for the 15m Autopilot strategy routes through this handler, the lowered `1.2` default in `GetDefaultStrategyConfig` is overwritten by `3.0` and the whole point of the config-driven floor is silently undone for that path. The plan must (a) confirm which create path builds the live 15m bot, and (b) if it is `handler_user.go`, update `:275` to `1.2` (or thread the new default). Not listed as a touched file; this is a missing dependency.

### 2.5 References to verify / minor line drift
- Plan/export cite the R/R hardcode as `engine_position.go:152`; it is the `if riskRewardRatio < 3.0 {` at `kernel/engine_position.go:138` in the current tree. Content is correct; line is drifted.
- Plan §3.3 says "runs right after the candidate prune in `GetFullDecisionWithStrategy`"; confirmed the correct insertion point is immediately after `pruneCandidateCoinsWithoutMarketData(ctx)` (`kernel/engine_analysis.go:262`), before `enrichVergexDataWithStrategy`. Accurate.
- Export/plan claim `applyAutopilotFullSizeOpen` is invoked again inside `executeOpen*` (overwriting the force-time call). **Confirmed**: `executeOpenLongWithRecord` calls `at.applyAutopilotFullSizeOpen(decision, equity)` at `trader/auto_trader_orders.go:155`, and the risk cap at `trader/auto_trader_risk.go:391` is gated on `StopLoss > 0 && TakeProfit > 0`. The entire §3.4 reordering premise is valid.
- Export references `executeOpen*` ordering `(:134-167)`; the SL/TP fill actually sits at `orders.go:196-200` (after quantity), and `applyAutopilotFullSizeOpen` at `:155`. Same content, line drift.

## 3. Details a named simpler design fully replaces (with justification)

### 3.1 Replace the `filterCandidatesByATRCap` "fallback `d.ATR14`" and the `fetchATR14` "`Data.ATR14`" with `TimeframeData[tf].ATR14`
Covered in §1.1. The simpler and correct source is the per-timeframe entry. Because `calculateTimeframeSeries` computes `data.ATR14` from the **full** kline slice (`data_klines.go:235`) rather than the `count`-limited tail, `TimeframeData["15m"].ATR14` is valid regardless of `count` — so the plan can drop its reliance on any top-level field and read the one correct location everywhere. No new export needed.

### 3.2 `writeModeVariant` / `scalping` variant: leave dormant — correct, no change needed
Plan/export correctly decide **not** to activate the dormant `scalping` variant (`auto_trader_loop.go:116` hardcodes `"balanced"`). Confirmed `writeModeVariant` maps `scalping` but it is unreachable in the loop. Keeping the loop on `"balanced"` and producing the scalp purely via config is the right, lower-risk call. No critique.

### 3.3 `auto_trader_risk.go` comment-only change is correct
The plan's decision to leave the `SL>0 && TP>0` gate (`auto_trader_risk.go:391`) unchanged and instead fill the stop earlier is the minimal correct fix. Confirmed the gate still needs to no-op the *force-time* `applyAutopilotFullSizeOpen` call (where no stop exists yet), and that the *execution-time* re-size then sees the ATR stop. No change required; flagging as a correctly-named simpler design.

## 4. Absent from both export and plan

### 4.1 `fetchATR14` testability
Plan §6 lists tests "prove each seam without network", but `fetchATR14` calls `market.GetWithTimeframes` directly and is not unit-testable as designed. Either inject the ATR lookup or accept that only `ensureStopLossTakeProfitDefaults` (with a passed-in `atr14`) is unit-tested and `fetchATR14` is covered by the manual/replay gate. Minor, but the plan's test list implies coverage that doesn't exist.

### 4.2 Failure of the ATR fetch at the eligibility boundary is asymmetric
The kernel filter and forced-open check are `fail-open` (missing data → coin admitted); the execution `fetchATR14` failure is `fail-to-fixed` (−3%/+8%). Both are stated, but neither doc notes the combined behavior: a high-ATR coin whose 15m ATR fetch transiently fails will (a) pass eligibility (fail-open) **and** (b) fall back to fixed −3%/+8% stops — i.e. exactly the pre-change behavior for the riskiest coins. Given the whole point is to cap high-ATR risk, the fail-open-on-missing-data for eligibility is worth an explicit operator-visible log at each occurrence (the plan logs skips but not admits), and the choice to fail-open vs fail-closed at eligibility should be a stated decision, not an incidental one.

### 4.3 Lifecycle of `ATREligibilityTimeframe` normalization vs selected timeframes
Covered in §2.3; the "normalize via `normalizeTimeframe`" step in `ClampLimits` (`store/strategy.go`) validates the string but does not reconcile it against `SelectedTimeframes`. Absent from both docs.

### 4.4 No test for the forced-open eligibility skip (integration-only)
The plan's test list includes "forced-eligibility skip", but the check lives in the `fill()` closure inside `ensureLongShortCoverage` (which requires a configured `strategyEngine` + populated `MarketDataMap`). The existing `auto_trader_force_test.go` tests pass `&kernel.Context{}` with empty `MarketDataMap`, which is exactly the fail-open branch. A meaningful unit test needs a `Context` with a populated `MarketDataMap` and a fake `DirectionalCandidates` — confirm that's feasible with the current test helpers (`baseForceTrader`) or the "forced-eligibility skip" test will silently exercise only the fail-open path.

## 5. Questions whose answers would materially change the design/order

1. **Which create path builds the live 15m Autopilot strategy — `GetDefaultStrategyConfig` or `api/handler_user.go:275`?** If the latter, the lowered `1.2` default never takes effect, and the R/R wiring change is dead until `:275` is also updated. (Determines whether the config-surface step must also touch `api/handler_user.go`.)
2. **Is the 15m eligibility ATR to be compared against `CurrentPrice` of the same fetched `Data`, or against the *primary* timeframe price?** The export pins "ATR% = ATR14/price×100" but the plan's `atrPct` helper is undefined on the denominator. The `Data.CurrentPrice` is the primary-TF close (== 15m close here, so usually identical), but this must be stated, not implied.
3. **Fail-open or fail-closed on eligibility when a candidate's ATR is missing at the cap boundary?** Currently incidental (fail-open). For a hard risk-exclusion feature, a deliberate choice — and the operator-visible logging that goes with it — is required (§4.2).
4. **Should the forced pool share the AI pool's `MarketDataMap` universe, or fetch ATR on demand?** The plan assumes overlap that may not hold (§2.2). If the operator wants true consistency, `fill()` must fetch per-candidate ATR; if not, the residual should be documented.
5. **Does the operator accept the `EnableATR` global-default flip** (`false→true`, plan's own Open Question #2)? This changes AI prompt token budget and output for all new strategies; the plan leaves it as a deploy-time confirmation but it materially changes default behavior.

---

## Recommendations (priority order)

1. **P0 — Fix the ATR source field everywhere.** Replace every `Data.ATR14` reference (top-level) in plan §3.3 and §3.4 with `TimeframeData[tf].ATR14`; drop the `d.ATR14` fallback in `filterCandidatesByATRCap`; re-spec `fetchATR14` to return `data.TimeframeData[tf].ATR14`. Otherwise both seams are compile errors / silent zeros.
2. **P0 — Fix the `ATRStopTarget` validity predicate** to be side-aware (§2.1), with a short-side degenerate unit test.
3. **P1 — Investigate `api/handler_user.go:275`** and include it in the config-surface step if it builds the live bot (§2.4).
4. **P1 — Resolve the forced-pool universe gap** (§2.2) and the eligibility-timeframe-vs-selected-timeframes guard (§2.3).
5. **P1 — Make the eligibility fail-open/fail-closed choice explicit** with operator-visible logging (§4.2).
6. **P2 — Confirm the forced-eligibility unit test is actually non-trivial** given the existing test helpers populate empty `MarketDataMap` (§4.4), and acknowledge `fetchATR14` is not unit-testable (§4.1).

The core execution-ordering diagnosis (§3.4), the R/R threading shape, the eligibility insertion point, and the prompt text changes are all validated against the code and remain correct.
