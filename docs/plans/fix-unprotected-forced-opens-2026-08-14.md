# Fix Unprotected Forced Opens (Missing SL/TP): Plan

## Goal

On the `dev` branch, fix the bug where a position can open without stop-loss/take-profit: forced opens (and any `kernel.Decision` with `StopLoss`/`TakeProfit` = 0) reach `SetStopLoss`/`SetTakeProfit` with trigger price 0, which Hyperliquid rejects ("Order has invalid price"), leaving the position unprotected. Ensure every open carries valid SL/TP; if placement still fails, surface it loudly instead of silently.

## Execution Index

| # | Work item | Goal | Done when | Key files | Deps | Size |
|---|---|---|---|---|---|---|
| 1 | Additive SL/TP helpers + unit tests | Derive + attach SL/TP; prove in isolation | `ensureStopLossTakeProfitDefaults` + `attachStopLossTakeProfit` + `auto_trader_sltp_test.go` green; no behavior change | `trader/auto_trader_orders.go`, `trader/auto_trader_sltp_test.go` (new) | none | M |
| 2 | Wire into the open path (atomic) | Every open fills + places SL/TP; failure is fatal | Insertions A+B in both `executeOpen*`; `actionRecord` updated; bridge test; `go test ./trader/...` green | `trader/auto_trader_orders.go` | 1 | M |

## 1. Summary

Add a **shared, code-enforced SL/TP fallback** inside `executeOpenLongWithRecord` / `executeOpenShortWithRecord` (the single chokepoint through which both forced and AI opens pass). When a decision carries `StopLoss<=0` **or** `TakeProfit<=0`, fill **both** from the entry price (`marketData.CurrentPrice`): stop **-3%**, target **+8%** (long `0.97x`/`1.08x`, short `1.03x`/`0.92x`). The fill runs **before** the position is opened (clean abort if entry price is unusable); the placement runs after. If `SetStopLoss`/`SetTakeProfit` still fail, the open returns an error so the caller records a failure instead of silently leaving the position. Targeted, additive change confined to `trader/auto_trader_orders.go` + tests; **no** changes to `provider/vergex`, `nofxos`, or `service/signal` (signal-service feature-branch work stays untouched — this lands on `dev`).

## 2. Current-State Analysis

### Open path (end-to-end)
```
runCycle (auto_trader_loop.go)
  ├─ AI decisions → sortDecisionsByPriority → filterDecisionsToStrategyUniverse
  ├─ ensureLongShortCoverage (auto_trader_force.go)  ← appends forced Decision (SL/TP = 0)
  └─ loop: executeDecisionWithRecord(&d, &actionRecord)  (loop.go:329-338)
        └─ executeOpenLongWithRecord / executeOpenShortWithRecord (auto_trader_orders.go L46/L162)
             ├─ GetPositions → enforceMaxPositions → dup-symbol check
             ├─ market.GetWithExchange(symbol)  → marketData.CurrentPrice   ← entry source
             ├─ balance/equity → applyAutopilotFullSizeOpen (leverage+size only, no SL/TP)
             ├─ enforcePositionValueRatio → margin affordability → enforceMinPositionSize
             ├─ quantity = actualPositionSize / marketData.CurrentPrice
             ├─ SetMarginMode → OpenLong/OpenShort
             ├─ recordAndConfirmOrder (auto_trader_decision.go:258)  ← position OPENED + DB-recorded
             ├─ SetStopLoss(... decision.StopLoss)   (L150-155 / L266-271)  ← logs error, does NOT return
             └─ SetTakeProfit(... decision.TakeProfit)                         ← logs error, does NOT return
        └─ hyperliquid SetStopLoss (trader_orders.go:933) / SetTakeProfit (:981)
             → price 0 → "Order has invalid price"
```

### Root cause
- `ensureLongShortCoverage` (auto_trader_force.go:96-103) builds forced `Decision` with only `Action/Symbol/Confidence/Reasoning`; `applyAutopilotFullSizeOpen` (auto_trader_risk.go:273-295) sets `Leverage`+`PositionSizeUSD` only. `StopLoss`/`TakeProfit` stay 0 (`kernel.Decision` engine.go:119-128).
- `executeOpen*` calls `SetStopLoss`/`SetTakeProfit` with 0; Hyperliquid rejects; the position is already open + recorded. The SL/TP block (auto_trader_orders.go:150-155/266-271) is log-and-continue, so failure is silent.
- `validateDecisions`/`validateDecision` (kernel/engine_position.go) already reject `SL<=0||TP<=0` + enforce R/R>=3:1, but it is **only** invoked from `parseFullDecisionResponse` (engine_analysis.go:282) — never on the trader execution path, so forced opens bypass it.

### Reusable / blocking
- Reusable: entry price already fetched (`marketData.CurrentPrice`); `Trader` interface already declares `SetStopLoss(symbol, positionSide string, quantity, stopPrice float64) error` / `SetTakeProfit(...)`; `at.logInfof` is the logging idiom; risk-enforcement helpers already guarantee caps.
- Blocking: `market.GetWithExchange` is package-level (not on `AutoTrader`) → `executeOpen*` not unit-testable without network. Factor the post-open placement into a separately testable `attachStopLossTakeProfit`.
- **Do-not-change boundary:** `trader/hyperliquid/trader_orders.go` (the setters). The fix is strictly upstream.

## 3. Design

### 3.1 Default-derivation helper (in `trader/auto_trader_orders.go`)

```go
const (
    defaultStopLossPct   = 0.03 // -3% stop from entry (matches vergexHoldRules)
    defaultTakeProfitPct = 0.08 // +8% target from entry (matches vergexHoldRules)
)

// ensureStopLossTakeProfitDefaults fills BOTH SL/TP from entryPrice whenever
// EITHER is <= 0. Returns an error only when entryPrice is unusable (<= 0) —
// callers abort the open before positioning. Never mutates a fully-specified pair.
func (at *AutoTrader) ensureStopLossTakeProfitDefaults(d *kernel.Decision, entryPrice float64) error
```

**Behavior (decision, fully resolved):**
- `d.StopLoss > 0 && d.TakeProfit > 0` → **no-op** (never override a fully-specified pair).
- `entryPrice <= 0` and defaults needed → return `fmt.Errorf("cannot derive default SL/TP from invalid entry price %.4f for %s", entryPrice, d.Symbol)`; no mutation.
- **If EITHER field is `<= 0`, fill BOTH from entry** (per critique P1.3, see §3.6). This guarantees the 8/3 ≈ 2.67:1 pair and prevents degenerate partial combinations (e.g., an AI-supplied wide stop + a derived tight target):
  - `d.Action == "open_long"`: `StopLoss = entry*0.97`, `TakeProfit = entry*1.08`
  - `d.Action == "open_short"`: `StopLoss = entry*1.03`, `TakeProfit = entry*0.92`
  - Unknown action (unreachable from open path): default to long signs.
- Log on any fill: `at.logInfof("Filled default SL/TP for %s from entry %.4f: SL=%.4f TP=%.4f", d.Symbol, entryPrice, d.StopLoss, d.TakeProfit)`.

**Sign convention (verified against Hyperliquid):** `SetStopLoss` with `positionSide=="SHORT"` sets `isBuy=true` → short stop buys-to-cover **above** entry (`1.03x` correct); long stop is a sell trigger **below** entry (`0.97x` correct). `TakeProfit` mirrors (`1.08x` above for long, `0.92x` below for short), preserving `SL > TP` for shorts (what `validateDecision` expects).

**Entry source assumption (explicit):** defaults derive from `marketData.CurrentPrice` (fetched before the aggressive IOC open, ~1% off the real fill). No extra API round-trip; document in a code comment.

### 3.2 Post-open placement helper (in `trader/auto_trader_orders.go`)

```go
// attachStopLossTakeProfit places both reduce-only trigger orders for an open
// position. Any failure returns immediately (position stays open on the exchange —
// caller records the error). Called only after ensureStopLossTakeProfitDefaults,
// so prices are always > 0.
func (at *AutoTrader) attachStopLossTakeProfit(symbol, side string, quantity, stopLoss, takeProfit float64) error
```
- `at.trader.SetStopLoss(symbol, side, quantity, stopLoss)`; on error return `fmt.Errorf("opened %s but failed to set stop loss at %.4f: %w", symbol, stopLoss, err)`.
- `at.trader.SetTakeProfit(symbol, side, quantity, takeProfit)`; on error return `fmt.Errorf("opened %s but failed to set take profit at %.4f: %w", symbol, takeProfit, err)`.
- One method removes the near-duplicate SL/TP blocks in both `executeOpen*` functions and gives a clean seam for stub-trader unit tests.

### 3.3 Wiring into `executeOpenLongWithRecord` / `executeOpenShortWithRecord`

**Insertion A — derive + record defaults (PRE-OPEN).** Immediately after the quantity calc + `actionRecord.Quantity/Price` assignments, **before** `SetMarginMode`/`OpenLong`/`OpenShort`:
```go
if err := at.ensureStopLossTakeProfitDefaults(decision, marketData.CurrentPrice); err != nil {
    return fmt.Errorf("aborting %s open without SL/TP: %w", decision.Symbol, err)
}
actionRecord.StopLoss = decision.StopLoss
actionRecord.TakeProfit = decision.TakeProfit
```
The `actionRecord` update is essential: `runCycle` builds `store.DecisionAction` at loop.go:300-315 with the **original** (possibly 0) SL/TP, then appends the passed-in pointer after execution (loop.go:339/343). Updating it here persists the **filled** values into saved decision history. No `runCycle` change required.

**Insertion B — replace the SL/TP placement block (L150-155 long, L266-271 short), POST-OPEN.** Delete the two `logger.Infof("  ⚠ Failed to set ...")` blocks; replace with:
```go
if err := at.attachStopLossTakeProfit(decision.Symbol, "LONG", quantity, decision.StopLoss, decision.TakeProfit); err != nil {
    return err
}
```
(short variant uses `"SHORT"`.) Because Insertion A ran pre-open, `decision.StopLoss/TakeProfit` are guaranteed `> 0`, so placement is never attempted at price 0.

### 3.4 Failure handling & caller tolerance

- **Derivation failure (`entryPrice <= 0`):** pre-open, returns error → **no position opened** (clean abort).
- **Placement failure (post-open):** the position is already open + DB-recorded. Returning an error **cannot unwind it** — an honest limitation of the design, not a solvable one. The contract: the fallback guarantees a non-zero trigger price is *attempted*, and a residual placement failure is surfaced as an error + failed decision record. **No compensating close in v1** (an auto-close mid-flow can itself fail and was not requested). The error bubbles to `runCycle`'s existing handler (loop.go:329-338): `at.logErrorf`, `actionRecord.Error` set, `actionRecord.Success` stays false, and the loop **continues** to the next decision.
- **Forced-open book management:** if placement fails, the position WAS opened on the exchange → next cycle `ensureLongShortCoverage` counts it in `ctx.Positions` → the direction is covered → no repeat open / no churn (the slot is not "freed and re-tried"). Desired outcome.
- **Realism note:** the common failure cause (price 0) is now impossible (defaults guarantee > 0); a residual placement failure is a genuine exchange/infra error surfaced loudly via log + decision record.

<!-- PLAN_MID -->

### 3.5 Edge cases (resolved)

| Case | Behavior |
|---|---|
| Entry `CurrentPrice <= 0`, defaults needed | Pre-open error → open aborted before `OpenLong`; no position. |
| Both SL/TP already `> 0` | Helper no-op; original values flow through; **never overridden**. |
| Partial (one set, one 0) | **Fill BOTH** from entry (critique P1.3) — prevents degenerate unvalidated opens. |
| Existing positions / manual adjust | Untouched — helper/attach only reachable from `executeOpen*` (open actions); the manual-adjust path (`GridTraderAdapter.PlaceLimitOrder`, types/interface.go:187-192) never calls them. |
| `hold`/`wait`/`close_*` | Never reach the fallback (only `open_long`/`open_short` route through `executeOpen*`). |
| `validateDecision` on execution path | **Not invoked** (see §3.6). |
| Configurable vs hardcoded | **Hardcoded consts** for v1 (see §3.6). |

### 3.6 Decisions on open questions

**Hardcoded consts (v1):** -3%/+8% pinned by the user and matching `vergexHoldRules` (engine_prompt.go:261), so prompt guidance and code-enforced behavior agree from one visible source. Avoids a `RiskControlConfig` schema addition + migration. Future config (if ever) adds `DefaultStopLossPct/DefaultTakeProfitPct` to `RiskControlConfig` — explicit future work, not now.

**Do NOT invoke `validateDecision` on the execution path.** Corrected reasoning (critique P1.2): the -3%/+8% defaults would actually **pass** the validator — its inferred entry `entry = SL + (TP-SL)*0.2 = 0.992e` gives risk 2.218%, reward 8.871%, R/R = **4.00:1 >= 3.0 → PASS**. The real reasons to keep `validateDecisions` in parse-only: (a) forced opens already satisfy position-value/leverage caps via `applyAutopilotFullSizeOpen` + `enforcePositionValueRatio`; (b) the validator measures R/R on an *inferred* entry, so adding it at execution would introduce a second, differently-measured rejection layer for AI opens. **Known pre-existing inconsistency (flag, out of scope):** the prompt+fallback target ~2.67:1 real R/R while the validator demands >=3:1 on its inferred entry; surfaced for a future decision (align validator to real-entry measurement).

**Partial-fill policy (resolved per critique P1.3):** when EITHER field is <=0, fill BOTH. This avoids the degenerate case (e.g., an AI-supplied 10% stop + a derived +8% target → ~0.8:1 real R/R, opened with nothing gating it). Option (a) — always yield the predictable 2.67:1 pair.

### 3.7 Concurrency & lifecycle

No new concurrency. `executeOpen*` runs synchronously in the single `runCycle` goroutine (sequential per-decision loop). The helper and `attachStopLossTakeProfit` are synchronous, lock-free, touching only per-iteration locals (`decision`, `actionRecord`) by pointer. The drawdown monitor runs on its own goroutine but never touches these. No cancellation semantics (calls complete or error normally).

### 3.8 Branch/scope discipline

Base off `dev`. Change is exclusively in the `trader` package (`auto_trader_orders.go` + tests). Does **not** touch `provider/vergex`, `nofxos`, or `service/signal` — zero overlap with signal-service feature-branch work. Confirm `auto_trader_orders.go` matches `dev` before editing.

## 4. File-by-File Impact

| File | Change | Why |
|---|---|---|
| `trader/auto_trader_orders.go` | **Modify.** Add consts `defaultStopLossPct`/`defaultTakeProfitPct` (near existing `marginOverheadFactor`/`takerFeeRate`); add `ensureStopLossTakeProfitDefaults` + `attachStopLossTakeProfit`. In both `executeOpen*`: insert Insertion A (fill + `actionRecord` update) after quantity calc / before `SetMarginMode`; replace the SL/TP block (L150-155 / L266-271) with the `attachStopLossTakeProfit` call returning on error. | Single shared chokepoint; covers forced + AI opens. |
| `trader/auto_trader_sltp_test.go` (NEW) | Unit tests (§6) + stub `types.Trader`. | Prove helpers without network. |
| `trader/auto_trader_force_test.go` | Modify (optional): bridge test forced→fallback. | Verify the handoff. |
| `kernel/*`, `hyperliquid/trader_orders.go`, `types/interface.go`, `store/strategy.go`, `market/types.go`, `auto_trader_loop.go`, `auto_trader_risk.go`, `auto_trader_force.go` (production) | **No change.** | Do-not-modify boundary (setters); fallback lives in execution path, not `applyAutopilotFullSizeOpen` (keeps autopilot sizing orthogonal). |
| `docs/plans/fix-unprotected-forced-opens-2026-08-14.md` | **No change** (as instructed). | — |

## 5. Risks & Migration

- **No persistence/schema changes** (hardcoded consts) → no migration; rollback = revert the single file change.
- **Behavior change on failure:** SL/TP placement errors go from non-fatal to fatal (open recorded as failed). Intentional — the core of the fix.
- **V1 residual (honest contract):** post-open placement failure leaves the position open/unprotected; error-only alerting, no compensating close. Documented.
- **Existing unprotected positions NOT retroactively protected** (e.g., SKHY) — only new opens. The drawdown monitor (`startDrawdownMonitor`, risk.go) remains the only safety net. Retroactive reconciliation is a separate follow-up, out of scope.
- **Accepted verification gap (critique P2.6):** `market.GetWithExchange` is unmockable, so the CI suite does NOT prove a forced open carries SL/TP end-to-end; only the extracted helpers + a bridge test are unit-covered. This gap is accepted (live verification is manual, per §6).

## 6. Tests & Verification

### Unit tests (`trader/auto_trader_sltp_test.go`, `baseForceTrader()`-style config helper)
1. `TestSLTPDefaultsLong`: `open_long`, entry=100, SL/TP=0 → SL=97, TP=108.
2. `TestSLTPDefaultsShort`: `open_short`, entry=100 → SL=103, TP=92.
3. `TestSLTPPartialFillFillsBoth`: `open_long`, SL=95, TP=0, entry=100 → SL=97, TP=108 (both filled per P1.3).
4. `TestSLTPNoOverrideWhenBothSet`: SL=95, TP=110, entry=100 → unchanged.
5. `TestSLTPEntryZeroErrors`: both 0, entry=0 → error returned, decision not mutated.
6. `TestAttachStopLossTakeProfitSuccess` (stub trader returns nil for setters → nil).
7. `TestAttachStopLossTakeProfitStopLossFailure` (stub `SetStopLoss` errors → error returned; `SetTakeProfit` not called).
8. `TestAttachStopLossTakeProfitTakeProfitFailure` (stub SL ok, TP errors → error returned).

**Stub trader for 6-8:** a minimal struct implementing `types.Trader` — small struct with two function fields for `SetStopLoss`/`SetTakeProfit` (overridable) and no-op for the rest. `attachStopLossTakeProfit` only touches `SetStopLoss`/`SetTakeProfit`, so the stub need not exercise `OpenLong`/`GetPositions`.

### Bridge test (force → fallback)
`TestForcedOpenDecisionGetsSLTP`: run `ensureLongShortCoverage` on a vergex_signal config + a directional candidate; take any appended forced decision, assert `StopLoss==0 && TakeProfit==0`, then `ensureStopLossTakeProfitDefaults(&d, 100)` → assert `d.StopLoss>0 && d.TakeProfit>0` and correct long/short signs. Replaces a live integration test (the full path calls the unmockable package-level `market.GetWithExchange`).

### Static checks
`go build ./trader/...`, `go vet ./trader/...`, `go test ./trader/...`.

### Verifying the SKHY-style incident is fixed (manual, Hyperliquid account)
1. Reproduce: let the book become one-directional so `ensureLongShortCoverage` appends a forced open (confirm via the `Forced ... (score ..., account-sized ...)` log).
2. Observe the open completes and the next two log lines show **non-zero** trigger prices: `Stop loss price set: <entry*0.97 or *1.03>` and `Take profit price set: <entry*1.08 or *0.92>`, with **no** `Failed to set stop loss ... Order has invalid price`.
3. No-regression on normal AI opens: an AI decision that supplies SL/TP still places its own values (logged) and is **not** overridden.
4. Post-deploy log sweep: `grep "Order has invalid price"` shows no occurrences originating from `SetStopLoss/SetTakeProfit` on opens.

## 7. Implementation Order

1. **Additive helpers + unit tests** — in `auto_trader_orders.go` add the two consts, `ensureStopLossTakeProfitDefaults`, `attachStopLossTakeProfit`; add `auto_trader_sltp_test.go` (tests 1-8) + stub. Additive-only; all unit tests green; no behavior change yet.
2. **Wire into the open path (ATOMIC)** — insert Insertion A + `actionRecord` update and replace the SL/TP blocks in both `executeOpen*`; add the bridge test. Build + vet + run all `trader` tests. Must land as one unit (fill + fatal placement together).

Steps 1 and 2 may be one commit for review clarity; step 2 must not be split internally.

**Assumptions (validate at implementation):** `at.logInfof` is available on `*AutoTrader` (else use `logger.Infof`); `executeOpen*` receives `actionRecord *store.DecisionAction` whose mutations persist into the saved record; `auto_trader_orders.go` matches `dev`.

## Open Questions (survived polish)

- None material remain. The partial-fill policy, hardcoded-vs-config, and validateDecision-invocation decisions are resolved (§3.6). Future work: configurable SL/TP percentages; aligning the validator's inferred-entry R/R with the real-entry measurement; retroactive SL/TP reconciliation for already-unprotected positions.
