# ATR-Scaled Trailing Stop + Profit Lock (15m Scalp): Plan

Date: 2026-08-17 · Branch: `feat/self-hosted-signal-stack` · Plan only — no code, no commit.

## Goal

Add an **ATR-scaled trailing stop** (break-even → trail) so the 15m scalp bot locks in profit on winning positions instead of holding to a fixed TP and giving back gains, and **fix the existing drawdown-close** so its "give back 40%" safety net actually fires for scalps (its +5% arming threshold is currently unreachable for +2–3% ATR targets).

### Confirmed decisions
1. **Approach**: both a trailing stop (proactive) **and** the drawdown-threshold fix (reactive).
2. **Shape**: break-even at **+1×ATR**, then trail SL at **1×ATR behind peak**. ATR = per-coin 15m ATR14.
3. **TP**: keep the fixed **2×ATR TP** as the primary target; the trail only ratchets the SL up to protect downside — **never moves TP**.
4. **Enforcement**: code-enforced exchange-side SL move (robust even if AI down) + a prompt nudge for AI profit-taking.

## Summary

Three additive pieces composed into the existing 1-minute drawdown monitor, building on the already-merged ATR-scalp exit scheme (do **not** re-touch `ATRStopTarget`, `ensureStopLossTakeProfitDefaults`, the ATR eligibility filter, or the ATR config fields):
- **(a)** a cancel+re-place SL/TP wrapper in the orders layer (Hyperliquid's `CancelStopOrders` cancels coin-wide, TP included, so TP must always be re-placed),
- **(b)** a new `checkTrailingStops` that ratchets the exchange-side SL to `peak ∓ 1×ATR14` (≥ break-even) and never touches TP,
- **(c)** an ATR-relative drawdown arming threshold replacing the flat 5.0%.

All logic runs inside the existing monitor ticker via `at.trader` — no `Trader` interface change, robust even if AI is down.

> **Critical validation vs. stale descriptions**: the planning agent's architecture notes claimed `EnableATR` defaults `false` and `vergexHoldRules` still carries −3%/+8% literals. The **actual files** show otherwise — `store/strategy.go` has `EnableATR: true` and `vergexHoldRules()` (`kernel/engine_prompt.go`) already reads *"stop-loss at 1.5× the coin's 15m ATR14… take-profit at 2×… R/R ≈ 1.33"* with no −3%/+8%. The prior ATR-scalp work is fully merged. Treat the **files as ground truth**; the only remaining prompt work is the optional momentum-stall profit-taking nudge.

## Current-state analysis

**Exit/SL architecture (end-to-end).**
- On open, `executeOpenLongWithRecord`/`executeOpenShortWithRecord` (`trader/auto_trader_orders.go`) derive ATR SL/TP **before** sizing via `ensureStopLossTakeProfitDefaults(d, entry, atr14)` → `kernel.ATRStopTarget(entry, atr14, atrStopMultiplier(), atrTargetMultiplier(), isLong)`; then `applyAutopilotFullSizeOpen` runs with a real stop; finally `attachStopLossTakeProfit(symbol, side, quantity, stop, tp)` (`:107`) calls `at.trader.SetStopLoss(...)` then `at.trader.SetTakeProfit(...)` — **place-once, no cancel, no updates**.
- `SetStopLoss`/`SetTakeProfit` (`trader/hyperliquid/trader_orders.go:1018/:1066`) each build a **new** reduce-only trigger order; they never modify/dedupe.
- `CancelStopOrders(symbol)` (`:411`) cancels **all** pending orders for the coin (SDK `OpenOrder` can't distinguish SL from TP → cancels TP too). `CancelStopLossOrders`/`CancelTakeProfitOrders` (`:363`/`:371`) forward to it. There is **no cancel+re-place wrapper and no trailing/break-even logic anywhere** in `trader/` (grep confirmed).
- **Drawdown monitor** (`trader/auto_trader_risk.go`): `startDrawdownMonitor` (`:37`) is a 1-minute `time.Ticker` goroutine (started `auto_trader.go:482`, stopped via `stopMonitorCh` + `monitorWg`). `checkPositionDrawdown` (`:61`) fetches `at.trader.GetPositions()`, iterates raw maps (`symbol`/`side`/`entryPrice`/`markPrice`/`positionAmt`/`leverage`, `:70-84`), computes `pricePnLPct` (`:90-100`), `currentPnLPct = pricePnLPct*leverage` (`:101`), `drawdownPct` from `peakPnLCache` (`:119-122`), and on hit calls `at.emergencyClosePosition` (`:147`) → `CloseLong/CloseShort`. It **does not touch exchange SL/TP**.
- Constants `drawdownClosePriceGainPct = 5.0` / `drawdownCloseGivebackPct = 40.0` (`:13-14`); `shouldDrawdownClose(pricePnLPct, drawdownPct)` returns `pricePnLPct > 5.0 && drawdownPct >= 40.0` (`:32`). The +5% arm is unreachable for ATR scalps (TP ≈ +2–3%).
- Peak bookkeeping: `peakPnLCache map[string]float64` keyed `symbol_side`, monotonic `UpdatePeakPnL` (`:182`), `ClearPeakPnLCache` (`:199`), guarded by `peakPnLCacheMutex`. **It stores margin-based (leveraged) P&L%, not price** — unusable for a price-based trail.

**ATR availability (verified).** `fetchATR14(symbol)` (`trader/auto_trader_orders.go:98`) already returns a coin's `TimeframeData[tf].ATR14` (tf from `atrTimeframe()`, default "15m") via `market.GetWithTimeframes(symbol, []string{tf}, tf, 30)`; returns 0 on failure/absent. **Not unit-testable** (direct package-level `GetWithTimeframes`). Top-level `market.Data` has **no** `ATR14` field — read only `TimeframeData[tf].ATR14`. Position cap is `MaxPositions` (2 in current default), so ≤2 ATR fetches/min — no rate concern.

**Reusable / blocking.**
- Reusable: `fetchATR14`; the `Trader` interface already declares `SetStopLoss`/`SetTakeProfit`/`CancelStopOrders`/`GetPositions`/`CloseLong`/`CloseShort`; the monitor's position loop, `peakPnLCache` mutex pattern, `stopMonitorCh`/`monitorWg`, and the `symbol_side` key convention.
- Blocking: `checkPositionDrawdown` fetches positions **internally** — calling a second per-tick loop would double-fetch; must refactor to fetch once. Current `peakPnLCache` is P&L%-based, not price — need a separate **peak-price** map for the trail. The currently-placed SL/TP is **not tracked anywhere** — need a source of truth to compare against (and the TP to re-place). `attachStopLossTakeProfit` receives `side` as "LONG"/"SHORT" while the monitor reads lowercase — key normalization required.

## Design

### 1. Config: `ATRTrailMultiplier` (`store/strategy.go`)

**Add the field** (not hardcode 1.0) — the ATR scheme is already parameterized; a hardcoded 1.0 breaks the file's convention. Additive field, mirroring the merged pattern:

```go
// in RiskControlConfig (after ATRTargetMultiplier)
ATRTrailMultiplier float64 `json:"atr_trail_multiplier"` // trail SL = peak ∓ this×ATR14; 0 = use default
```
- **Consts:** `MinATRTrailMultiplier = 0.5`, `MaxATRTrailMultiplier = 10.0`.
- **`ClampLimits`:** when `> 0`, clamp to `[min,max]`; `0` = "use default" (same convention as the other multipliers).
- **`GetDefaultStrategyConfig`:** `ATRTrailMultiplier: 1.0`.
- **`StrategyClampWarnings`:** add `appendFloat("ATR Trail Multiplier", "atr_trail_multiplier", ...)` for UI parity.
- Legacy configs read `0` → accessor `atrTrailMultiplier()` defaults to **1.0**.
- *(Scope note: the export left "hardcode vs. config field" as an open question; this plan resolves it to a config field to match the existing `ATRStopMultiplier`/`ATRTargetMultiplier` convention — a new decision, not a confirmed requirement.)*

### 2. Pure trail math: `kernel.TrailStopTarget` (`kernel/engine_position.go`)

Sibling of `ATRStopTarget`, sharing its side-aware sign mirroring (per the critique doc's short-side warnings):

```go
func TrailStopTarget(entry, peak, atr14, mult float64, isLong bool) (trail float64, ok bool)
```
- Long: arm when `peak >= entry + mult·atr14`; `trail = peak − mult·atr14` (auto ≥ entry). `ok=false` when `atr14 <= 0` or `trail <= 0`.
- Short: arm when `peak <= entry − mult·atr14`; `trail = peak + mult·atr14` (auto ≤ entry). `ok=false` when `atr14 <= 0` or `trail <= 0`.
- **Do not** include "max/min against current SL" here — that's trader plumbing (§4). Keep this pure.
- Rationale: arm-at-+1×ATR and trail-at-1×ATR-behind-peak collapse into one continuous rule (`trail = peak ∓ ATR`, which equals `entry` at the instant of arming), so no separate break-even branch is needed.

### 3. State: peak-price map + trail-state map (`trader/auto_trader.go`)

Two new fields on `AutoTrader` (init in `NewAutoTrader`):
```go
peakPrice    map[string]float64         // symbol_side -> best (max long / min short) mark price
trailState   map[string]trailingStopState // symbol_side -> currently-placed SL/TP (source of truth)
peakPriceMu  sync.RWMutex
trailStateMu sync.Mutex

type trailingStopState struct {
    stop float64 // currently-placed exchange SL
    tp   float64 // currently-placed exchange TP (never moved; re-placed as-is)
}
```
- **`peakPrice` seeding** (first observation) `= current markPrice`; monotonic long `max` / short `min`.
- **`trailState` seeding** — two paths:
  1. **Primary (open-time):** `attachStopLossTakeProfit` records `{stop, tp}` right after placing them.
  2. **Fallback (restart, position already open):** `checkTrailingStops` lazily seeds `{stop, tp} = ATRStopTarget(entry, atr14, atrStopMultiplier(), atrTargetMultiplier(), isLong)` when no entry exists. **Assumption (documented):** NOFX is the sole trader for these positions, so the on-exchange SL/TP is ATR-derived; the trail only ever *tightens*, so the residual effect is tightening a manual wider stop — acceptable in this single-bot setup. If `atr14` is unavailable at seed time, **take no action this tick**.
   - **"Never moves TP" is scoped to the open-time seed path.** The fallback seed **re-derives** TP from `ATRStopTarget` (a one-time recompute, equal to the real TP for an ATR-derived open; a manually-widened TP would be narrowed on the first trail move). After seeding, TP is never touched again. Document this in a code comment.

### 4. Cancel+re-place wrapper + accessor (`trader/auto_trader_orders.go`)

```go
func (at *AutoTrader) moveTrailingStopLoss(symbol, side string, quantity, newStop, tp float64) error
```
Sequential: `at.trader.CancelStopOrders(symbol)` → `at.trader.SetStopLoss(symbol, side, quantity, newStop)` → `at.trader.SetTakeProfit(symbol, side, quantity, tp)`. Any error returns immediately with a descriptive wrap; on failure the recorded state is **not** updated (next tick re-attempts). Add `atrTrailMultiplier()` accessor (`0`→1.0).

**Side case (critical):** the exchange contract is uppercase-sensitive — `SetStopLoss`/`SetTakeProfit` compute `isBuy := positionSide == "SHORT"` (`trader/hyperliquid/trader_orders.go:1021/:1071`). The monitor reads lowercase `"long"`/`"short"` from `GetPositions()`. `moveTrailingStopLoss` must pass **uppercase** `"LONG"`/`"SHORT"` to the exchange — take an `isLong bool` (or uppercase the incoming side) and never forward the lowercase monitor string. A lowercase `"short"` would place a *sell* trigger order for a short (the reversed-leg bug). This is the short-side correctness seam the merged critique warned about. Every existing call site already passes uppercase (`auto_trader_orders.go:254/:379`).

**Documented risk:** after `CancelStopOrders` there is a brief unprotected window before the new SL lands (inherent to Hyperliquid's coin-wide cancel; window is two sequential SDK calls). On wrapper failure, do **not** attempt a compensating close — the position keeps its prior SL or has a new SL but possibly no TP; error surfaces via logger, state stays stale, next tick retries.

### 5. Drawdown monitor refactor (`trader/auto_trader_risk.go`)

Refactor the ticker to **fetch positions once + build an ATR cache**, then dispatch to both functions:
```go
positions, err := at.trader.GetPositions()
if err != nil { log; continue }
atrCache := map[string]float64{}            // symbol -> ATR14, filled once
at.checkPositionDrawdown(positions)         // signature change () -> (positions)
at.checkTrailingStops(positions, atrCache)  // new
```
- `checkPositionDrawdown` **signature change** `()` → `(positions []map[string]interface{})`; drop its internal `GetPositions()`. (Ticker is the sole caller — safe.)
- `atrCache` helper `atrForSymbol(...)` → `fetchATR14(symbol)` once per symbol per tick. `MaxPositions=2` → ≤2 fetches/min; no cross-cycle TTL needed.

**ATR-relative drawdown arming** — replace the flat 5.0:
```go
func (at *AutoTrader) drawdownArmPct(entryPrice, atr14 float64) float64 {
    if entryPrice > 0 && atr14 > 0 {
        pct := atr14 / entryPrice * 100
        if pct >= 1.0 { return pct }   // 1×ATR, min 1%
    }
    return 1.5                          // ATR unavailable -> fixed floor
}
func shouldDrawdownClose(pricePnLPct, armPct, drawdownPct float64) bool {
    return pricePnLPct > armPct && drawdownPct >= drawdownCloseGivebackPct
}
```
Keep `drawdownCloseGivebackPct = 40.0` unchanged. **Rationale for ATR-relative over a fixed lower %:** it coin-scales and matches the ship-wide `1×ATR` theme; the >1.0% floor prevents degenerate near-zero-ATR arming.
- **Debug-log branch:** the removed `drawdownClosePriceGainPct` is also referenced by the `else if pricePnLPct > drawdownClosePriceGainPct` telemetry branch (`auto_trader_risk.go:138`). Re-express it against the per-position `armPct` (so the "close to condition" log still covers the new 2–3% scalp range) rather than deleting it or leaving it against a dead constant.
- **Two independent `1×ATR` thresholds (intentional):** the trail arms via `atrTrailMultiplier` (configurable, default 1.0) while `drawdownArmPct` hardcodes 1×ATR and is not configurable. They serve different purposes (profit-lock vs. giveback safety net) and are left independent; if a user raises `atrTrailMultiplier` the drawdown close still arms at 1×ATR. Note this in a comment.

### 6. Trail loop: `checkTrailingStops(positions, atrCache)` (`trader/auto_trader_risk.go`)

Same 1-minute cadence, iterating the same positions. Per position:
1. **Priority vs drawdown:** drawdown runs first; positions it closed are skipped here (track closed symbols; avoid placing SL then immediately closing).
2. **Peak update:** `peak = max(peak, mark)` long / `min(peak, mark)` short (first-seen seed = mark).
3. **ATR:** `atr14 := atrForSymbol(...)`; if `atr14 <= 0` → **no trail action this tick** (fail-closed).
4. **Seed `trailState`** if absent (§3 fallback); if seed fails, skip.
5. **Compute candidate:** `if trail, ok := kernel.TrailStopTarget(entry, peak, atr14, at.atrTrailMultiplier(), isLong); ok` → long `newStop = max(current.stop, trail)`, short `newStop = min(current.stop, trail)`.
6. **Epsilon gate:** act only when `math.Abs((newStop - state.stop) / entry) > 1e-4` (avoid churn; never re-place an unchanged SL).
7. **Move:** `at.moveTrailingStopLoss(symbol, side, quantity, newStop, state.tp)`; on success `trailState[symbol_side].stop = newStop`. **`state.tp` is always the recorded open TP — never recomputed, never moved.**
8. **GC:** prune `trailState`/`peakPrice` keys not in the current position set (handles every close path). This GC is the **single cleanup owner** — do not also add an explicit `trailState`/`peakPrice` clear in `emergencyClosePosition` (the position vanishes from `GetPositions()` and the GC reclaims it next tick); the existing `ClearPeakPnLCache` (margin-PnL) remains separate and unchanged.

**Sign/key conventions:** key = `symbol + "_" + strings.ToLower(side)`; a tiny `positionKey(symbol, side)` helper (lowercases side) reused by the open-seed and the monitor. `attachStopLossTakeProfit` receives "LONG"/"SHORT" → normalize via `positionKey`.

**Concurrency & lifecycle:** `peakPrice`/`trailState` are accessed from **two goroutines** — the monitor ticker and the autopilot open path (`executeOpen*WithRecord` seeds `trailState` via `attachStopLossTakeProfit`). The mutexes are **required** (an open can land mid-tick), not merely protective. `checkTrailingStops` must read/write `trailState` under `trailStateMu`. `stopMonitorCh`/`monitorWg` unchanged.

### 7. Prompt nudge (`kernel/engine_prompt.go`)

`vergexHoldRules()` is **already ATR-scaled** — do **not** reword. **Add only** a momentum-stall profit-taking nudge bullet (shared by both `zh`/`en` branches):
> When a position is ≥ +1.5×ATR in profit and momentum stalls (price no longer extending, volume fading), lean toward closing and banking the gain rather than holding to the 2×ATR target; the stop-loss auto-trails 1×ATR behind peak as the downside backstop.

Keep it a **nudge, not a hard rule**. `EnableATR` is already `true` and `SelectedTimeframes` includes `"15m"` — no prompt-data plumbing needed.

## File-by-file impact

| File | Change | Why | Deps |
|---|---|---|---|
| `store/strategy.go` | **Modify.** Add `ATRTrailMultiplier` + `Min/MaxATRTrailMultiplier` consts; `ClampLimits` + `StrategyClampWarnings`; default `1.0`. | Parameterize the trail; additive JSON. | none |
| `kernel/engine_position.go` | **Modify.** Add pure `TrailStopTarget` next to `ATRStopTarget`. | Single source for side-aware trail sign. | none |
| `trader/auto_trader.go` | **Modify.** Add `peakPrice`/`trailState` maps + mutexes + `trailingStopState`; init in `NewAutoTrader`. | Price-peak tracking + placed-SL/TP source of truth. | none |
| `trader/auto_trader_orders.go` | **Modify.** Add `moveTrailingStopLoss`; seed `trailState` in `attachStopLossTakeProfit`; add `atrTrailMultiplier()` + `positionKey()`. | Cancel+re-place primitive; open-time seed; key normalization. | §1, §3 |
| `trader/auto_trader_risk.go` | **Modify.** Ticker refactor (single fetch + ATR cache); `checkPositionDrawdown(positions)` + `drawdownArmPct` + `shouldDrawdownClose(..., armPct, ...)`; `checkTrailingStops`; peak tracking; GC; emergency-close state clear. | Trail + drawdown fix live here. | §2, §3, §4 |
| `kernel/engine_prompt.go` | **Modify.** Append momentum-stall nudge to `vergexHoldRules()`. | Optional; text already ATR-scaled. | none |
| `trader/types/interface.go` | **No change.** | `SetStopLoss`/`SetTakeProfit`/`CancelStopOrders` already declared. | — |
| `trader/auto_trader_throttle.go` | **No change.** | Close gates remain; `emergencyClosePosition` intentionally un-gated. | — |
| Tests | **Add.** `store` default+clamp; `kernel` `TrailStopTarget` long/short/degenerate; `trader` `shouldDrawdownClose` ATR-relative, `drawdownArmPct` floor/fallback, `moveTrailingStopLoss` order + TP-re-place + **uppercase side** (stub), `checkTrailingStops` arm/trail/epsilon/long/short/no-op/missing-ATR (injected cache). **Rewrite `trader/auto_trader_risk_test.go`** (the 2-arg `shouldDrawdownClose` call at `:22` breaks under the 3-arg form; preserve its "+0.5% must never arm" intent under ATR-relative arming). Add a partial-failure test: `CancelStopOrders`+`SetStopLoss` succeed, `SetTakeProfit` fails → state not updated → next tick retries. | Prove each seam without network. | each step |

## Execution index

| # | Work item | Goal | Done when | Key files | Deps | Size |
|---|---|---|---|---|---|---|
| 1 | Config surface | `ATRTrailMultiplier` config field | Field + consts + clamp + default; accessor `atrTrailMultiplier()` returns 1.0 | `store/strategy.go`, `trader/auto_trader_orders.go` (accessor) | none | S |
| 2 | Pure trail math | Side-aware trail level | `kernel.TrailStopTarget` + tests pass | `kernel/engine_position.go` | #1 | S |
| 3 | State fields | Price-peak + placed-SL/TP tracking | `peakPrice`/`trailState`/mutexes added + inited | `trader/auto_trader.go` | none | S |
| 4 | Wrapper + open-seed | Cancel+re-place primitive + state seed | `moveTrailingStopLoss` works; `attachStopLossTakeProfit` seeds `trailState` | `trader/auto_trader_orders.go` | #1, #3 | M |
| 5 | Monitor refactor + trail (core) | Trail ratchets SL; drawdown arms at 1×ATR | `checkTrailingStops` + `drawdownArmPct` + `shouldDrawdownClose(..., armPct, ...)`; ticker single-fetch; GC | `trader/auto_trader_risk.go` | #2, #3, #4 | L (atomic) |
| 6 | Prompt nudge | AI leans to close on momentum stall | Nudge bullet in `vergexHoldRules()` | `kernel/engine_prompt.go` | none | S |
| 7 | Full gate | Build/test/gofmt + manual replay | `go build ./...`, `go test ./trader/... ./kernel/... ./store/...`, gofmt; replay shows SL ratchet + TP untouched | all | #1–#6 | M |

## Implementation order

1. **Config** (`store/strategy.go` + `trader/auto_trader_orders.go` accessor) — add `ATRTrailMultiplier`, consts, clamp, warnings, default 1.0.
2. **Pure trail math** (`kernel/engine_position.go`) — `TrailStopTarget` + tests. *Atomic with test additions.*
3. **State fields** (`trader/auto_trader.go`) — `peakPrice`/`trailState` maps + mutexes + `trailingStopState`; init. Compiles standalone.
4. **Wrapper + seed** (`trader/auto_trader_orders.go`) — `moveTrailingStopLoss`, `positionKey()`; seed `trailState` from `attachStopLossTakeProfit`. Additive-only.
5. **Monitor refactor + trail** (`trader/auto_trader_risk.go`) — **atomic core**: single-fetch ticker + ATR cache; `checkPositionDrawdown(positions)` + ATR-relative arming; `checkTrailingStops`; peak tracking; GC; emergency-close clear. Lands as one unit with #4.
6. **Prompt nudge** (`kernel/engine_prompt.go`) — momentum-stall bullet. Independent.
7. **Full gate** — build/test/gofmt + manual replay (trail ratchets SL from ATR-open-stop → break-even → 1×ATR-behind-peak; TP never moves; drawdown arms on +2–3% scalp that gives back 40%).

## Verification

- `grep -n "drawdownClosePriceGainPct\|shouldDrawdownClose" trader/auto_trader_risk.go` → flat 5.0 gone, replaced by `drawdownArmPct`; 40% giveback retained.
- `checkTrailingStops` + `moveTrailingStopLoss` exist; SL moves to entry at +1×ATR then trails 1×ATR behind peak; re-places TP after every `CancelStopOrders`; side-aware.
- `vergexHoldRules` contains the nudge and no −3%/+8% literal (already true); `EnableATR: true` + `"15m"`.
- No `Trader` interface change.
- `go build ./...`; `go test ./trader/... ./kernel/... ./store/...`; gofmt clean; existing `atr_scalp_test.go`, `auto_trader_sltp_test.go`, `engine_prompt_test.go` pass.

## Risks & migration

- **No schema migration** — `atr_trail_multiplier` additive; legacy `0` → accessor 1.0. Rollback = revert.
- **Restart seed assumption** — fresh process with a position already open seeds `trailState` from ATR-derived SL/TP; trail only *tightens*, so a manual wider stop could tighten to ATR on first armed move. Accepted in single-bot setup; documented.
- **Unprotected cancel window** — `CancelStopOrders` → `SetStopLoss` → `SetTakeProfit` has a brief no-SL gap; inherent to Hyperliquid. On failure state stays stale → retried next tick.
- **Asymmetric fail behavior** — trail is fail-closed on missing ATR (no ratchet without volatility); drawdown is fail-open (fixed 1.5% floor). Deliberate: over-tightening SL is worse than not arming a safety close.
- **`checkPositionDrawdown` signature change** is internal-only (ticker sole caller).
- **Trail only tightens, never un-arms** — if peak retraces, trailing stop holds at last level (correct profit-lock semantics).

## References

- `trader/auto_trader_risk.go` — drawdown monitor (`:37`, `:61`, `:90-122`, `:147`, `:182`, `:199`, `:211`), constants (`:13-14`), `shouldDrawdownClose` (`:32`).
- `trader/auto_trader_orders.go` — `attachStopLossTakeProfit` (`:107`), `fetchATR14` (`:98`), `atrTimeframe` (`:76`), `ensureStopLossTakeProfitDefaults` (`:35`), `kernel.ATRStopTarget` (`:43`).
- `trader/hyperliquid/trader_orders.go` — `SetStopLoss` (`:1018`), `SetTakeProfit` (`:1066`), `CancelStopOrders` (`:411`), `CancelStopLossOrders`/`CancelTakeProfitOrders` (`:363`/`:371`).
- `trader/auto_trader.go` — monitor start (`:482`), struct peak-cache fields (`:195-199`).
- `trader/types/interface.go` — `SetStopLoss`/`SetTakeProfit`/`CancelStopOrders`/`GetPositions`/`CloseLong`/`CloseShort`.
- `store/strategy.go` — `RiskControlConfig` ATR fields (`:947-1006`), clamps (`:142-170`), defaults (`:1097-1103`).
- `kernel/engine_position.go` — `ATRStopTarget` (`:44`).
- `kernel/engine_prompt.go` — `vergexHoldRules` (`:263-268`).
- `market/data.go:148`, `market/data_klines.go:235`.
