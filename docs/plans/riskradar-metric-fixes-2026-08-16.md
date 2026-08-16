# RiskRadar Metric Fixes: Plan

Date: 2026-08-16 · Branch: `feat/self-hosted-signal-stack` · Scope: frontend only, `web/src/components/terminal/RiskRadar.tsx` (+ no other files)

## Goal

Fix the three degenerate/misleading RiskRadar metrics — **Leverage** (always "Risky"/full-red), **Concentration** (always "Concentrated"), **Drawdown** (any positive → "Caution") — so the panel reflects real risk and stops crying wolf. **Do not commit**; deliverable is this plan.

## Background (verified)

### Current RiskRadar logic (`web/src/components/terminal/RiskRadar.tsx`)
- **Leverage**: `configMax = max(btc_eth_leverage, altcoin_leverage)`; `levUse = avg/cap×100`; `>80 → Risky(red) / ≥50 → High / else Safe`. Because the autopilot pins every position to exactly the cap, **avg=cap** on a single-tier book → `levUse=100%` → **always Risky, always full red**. Degenerate. (Note: in a mixed-tier config, e.g. BTC 10× + altcoin 3×, `avg/cap` can be <100% — not *always* degenerate, but still misleading on a 3×-at-cap altcoin.)
- **Concentration**: `concentration = topNotional/totalNotional×100`; `≥35 → Concentrated / else Spread`. With `MaxPositions=2` + full-size, min concentration is 50% (2 balanced) or 100% (1 position) → **always ≥35 → always Concentrated; "Spread" unreachable**. Degenerate-ish.
- **Drawdown**: `drawdown = fullStats.max_drawdown_pct`; `≤0 → Calm / ≥20 → Deep / else Caution`. **Any positive drawdown (even 2.3%) → Caution; "Calm" only at exactly 0.** Over-sensitive.
- **Fine (no change)**: Net exposure (bias skew), Margin used, Positions (count vs cap).

### Verified data facts (backend)
- `max_drawdown_pct` is a **positive percent** (≥0), computed as `(peak−equity)/peak×100` (`store/position_query.go:204,218`), served by GET `/statistics/full` (`api/handler_stats_full.go:49`). RiskRadar must treat it as positive magnitude (it already negates for display).
- **`liquidation_price` can be 0** — Hyperliquid `LiquidationPx` is a pointer that is nil for **cross-margin** (`trader/hyperliquid/trader_positions.go:50-52,68`); so per-position liq-distance is unreliable.
- **Per-position `margin_used` does NOT exist** in `GetPositions` output (`trader_positions.go` emits no `margin_used`). Only **account-level `margin_used_pct`** from `/account` is available — a derived estimate (`trader/auto_trader_decision.go:166-189`), 0 when flat.
- `Position` type (`web/src/types/trading.ts:39-50`) has `liquidation_price` and `margin_used` fields, but `margin_used` will be `undefined` per-position; use the account-level `margin_used_pct` instead.

### User decisions (confirmed — "use your recommendations")
1. **Leverage verdict**: make the Leverage row **info-only** — show avg/peak/cap and an **"At cap"** note when `avgLev ≈ configMax`; no risk color (the **Margin Used** row owns the risk color, avoiding a duplicated gauge). Rejected: coloring leverage by margin% (duplicates Margin Used); per-position liq distance (unreliable on cross-margin).
2. **Concentration**: **Concentrated only when top position >70% of total, or exactly 1 position**; else Spread.
3. **Drawdown bands**: **<5% → Calm/Low, 5–20% → Caution, >20% → Deep**.

## Design

> **Criteria visibility (confirmed):** expose each metric's threshold bands as a **hover tooltip** on its verdict tag (`title` attribute on `Tag`, criteria baked into each `Verdict.title`) — no always-on legend, so the dense terminal layout is unchanged.

### `web/src/components/terminal/RiskRadar.tsx`

**1. Leverage row — make it info-only; the Margin Used row owns the risk color.**
- Because leverage is pinned to the cap by design, the leverage row carries no independent risk signal — coloring it by `marginPct` would duplicate the adjacent **Margin Used** row (same bar + verdict). So:
  - Keep the leverage numbers as info text (`3.0× avg / 3× peak · 3× cap`), unchanged.
  - Replace the red/amber/green risk verdict with an **"At cap"** note (muted/amber) when `avgLev ≈ configMax` (the common autopilot state), and `Safe`/muted otherwise. **No risk color on this row.**
  - The gauge bar for Leverage is dropped or left neutral; the **Margin Used** row remains the real risk-color signal (`utilColor(marginPct)`).
  - Flat guard: `avgLev === 0 || totalNotional === 0 → { text: '—', tone: 'muted' }` (so flat shows "—", not a misleading color).

**2. Concentration — match the MaxPositions=2 design.**
- Threshold: `concentration > 70 || count === 1 → Concentrated`, else `Spread`. (With 2 balanced positions ≈50% → Spread reachable.)
  ```js
  const concTag: Verdict =
    m.totalNotional === 0 ? { text: '—', tone: 'muted' }
    : (m.concentration > 70 || m.count === 1) ? { text: 'Concentrated', tone: 'amber' }
    : { text: 'Spread', tone: 'up' }
  ```

**3. Drawdown — add a low band; treat as positive magnitude.**
- Keep `drawdown = fullStats.max_drawdown_pct` (positive). Display already negates (`-${pct}`). New bands:
  ```js
  const ddTag: Verdict =
    m.drawdown <= 5 ? { text: 'Calm', tone: 'up' }
    : m.drawdown >= 20 ? { text: 'Deep', tone: 'dn' }
    : { text: 'Caution', tone: 'amber' }
  ```
  - A 2.3% drawdown now shows **Calm** (green), not Caution.

**4. Guards / edge cases.**
- **Flat (no positions)**: leverage/margin/concentration show "—"/muted (already handled by `avgLev===0`/`totalNotional===0`). Keep.
- **`margin_used_pct` missing or 0 when flat**: account `margin_used_pct` is 0 when flat — levTag would show "Safe" at 0, which is correct (no exposure). If `account` is null entirely, `hasData` already returns the "No live risk data." fallback.
- **Mixed account data**: `marginPct` already falls back to `(marginSum/equity)×100` when `margin_used_pct` is null (`RiskRadar.tsx` current logic) — keep that fallback; note per-position `margin_used` is undefined so the fallback effectively won't contribute and we rely on account `margin_used_pct`.

## File-by-file impact

| File | Change |
|------|--------|
| `web/src/components/terminal/RiskRadar.tsx` | 3 verdict blocks (leverage→margin %, concentration threshold, drawdown bands) + leverage gauge fill → `marginPct`. No other files. |

No backend, no store, no API changes. No changes to Net exposure / Margin used / Positions rows.

## Edge cases
- Flat (no positions): leverage "—" (guard `avgLev===0 || totalNotional===0`); concentration "—"; drawdown shows the historical stat.
- Cross-margin position with `liquidation_price=0`: not used by the new logic → no false "Safe" from missing liq data.
- Single position: concentration 100% → "Concentrated" (correct).
- Two balanced positions (~50%): concentration 50% ≤70% and count=2 → "Spread" (reachable again — the fix's goal).
- **Drawdown semantics**: `max_drawdown_pct` is the all-time historical peak-trough stat (monotonic), so "Calm" means "historical worst was shallow", not "current risk is low". Bands avoid flagging tiny drawdowns as Caution; do NOT present it as a live alarm.

## Verification
- `cd web && npx tsc --noEmit` — must pass (pure TS logic change).
- Unit test: extract `levTag/concTag/ddTag` into exported pure helpers and add a small Jest test covering the flat-state, single-position, two-balanced-position, and drawdown-band cases (this panel has no test today; lock in the threshold changes).
- Manual: load the terminal with (a) 1 hedged 3× position → Leverage "At cap", Concentration "Concentrated", Drawdown "Calm" (in demo, drawdown is hardcoded 5.2 → "Caution", so expect Caution on the demo toggle); (b) 2 balanced positions → Concentration "Spread"; (c) flat → Leverage "—".
- `grep` that no other component depends on the removed behavior (only RiskRadar uses these verdicts).

## Open questions (none material — decisions locked)
- Exact amber color / label wording for "High"/"Calm" (cosmetic, follow existing `C_AMBER`/tone conventions).

## References
- `web/src/components/terminal/RiskRadar.tsx` (full)
- `web/src/types/trading.ts:39-50` (`Position`)
- `store/position_query.go:204,218` (`max_drawdown_pct` sign)
- `trader/hyperliquid/trader_positions.go:50-52,68` (liq price 0 on cross)
- `trader/auto_trader_decision.go:166-189,221-222` (account margin_used_pct)
