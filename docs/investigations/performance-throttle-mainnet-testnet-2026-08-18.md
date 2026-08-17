# Investigation: Poor scalping performance — throttle noise-band vs mainnet-data/testnet-execution mismatch

## Summary
Two compounding flaws explain the losses: **(H1)** the trade-throttle exit gates (`trader/auto_trader_throttle.go`) were replay-tuned for a longer-hold strategy and now sit exactly on the 15m ATR scalp's P/L zone, so the AI's small loss-cuts are blocked and bleed into full exchange-SL hits; **(H2)** every input the strategy sees (signals, ATR14, SL/TP) is computed from MAINNET Hyperliquid while execution is on TESTNET, so entry/SL levels and fills are wrong by construction. H1 is the dominant per-close loss mechanism; H2 is the systemic correctness flaw.

## Symptoms
- Recent closes are structurally negative: ~3 winners (+$15.17, +$10.35, +$1.47) vs 4 losers (-$13.18, -$9.54, -$14.02, -$8.49) ≈ -18 USDT net before fees over ~7 round-trips; equity 698 → 673.5.
- XAI short: AI issued `close_short` at -0.86% (05:25) but was blocked by trade throttle ("inside noise band -2.0%..3.0%, wait ~20m"); 12 min later hit SL at -$8.49.
- HMSTR short: 0m-hold instant SL (-$9.54) — opened then immediately stopped.
- CASHCAT: repeated open failures ("Order could not immediately match against any resting orders" = no liquidity on testnet).
- Winning positions are smaller than losing ones; win rate ~43%, R:R ~1:1 (needs >50% at 1:1).

## Background / Prior Research

### Git archaeology — throttle noise-band origin
- The noise-band/throttle logic evolved across ~5 commits (Jul 21–26, 2026), driven by fee-driven drawdown analysis then a decision-replay tuning pass.
- `39eac5ac` (2026-07-21) "config: stop the churn — hold for big moves, wide TP/SL, low leverage": origin of the wide noise-band regime. Rationale: "Death by small-move grinding … the ~0.14% round-trip fee ate 30-50% of every tiny winner. The AI was closing positions on ±0.5% noise after the 60m min-hold, capping winners at ~0.86%." Widened bypass/floor/ceiling, min-hold 60m→4h, noise-close 90m→8h, reentry 30m→3h, opens/hour 30→3, opens/cycle 6→2.
- `574ddfb1` (2026-07-26) "revert: drop exit-gate configurability, hardcode the replay-validated values": source of the CURRENT constants. "set to the replay-validated values … (4154 cycles, 3-fold robust search over only the 7 exit params) … gates beat no-gates by 34 pts and the old rigid 4h/8h by 16 pts of worst-fold score." Also "Re-entering a just-closed symbol was a consistent loss source … cluster tightly at 3.8-4.0h" → `autopilotReentryCooldown = 4h`.
- `5cd62e3c` (2026-07-24) "fix: exit-guard thresholds were margin-basis": flagged that thresholds compared price-move % against leverage-multiplied margin PnL%.
- Current constants live in `trader/auto_trader_throttle.go`: `autopilotMinHoldDuration` L26 (90m), `autopilotNoiseCloseHoldDuration` L27 (3h), `earlyCloseStopLossBypassPct` L31 (-3.0), `earlyCloseTakeProfitBypassPct` L32, `noiseCloseLossFloorPct` L33 (-2.0), `noiseCloseProfitCeilingPct` L34 (+3.0), `autopilotReentryCooldown` (4h). `closeThrottleReason` at L176.
- **Critical flag:** `docs/plans/scalp-15m-atr-tpsl-2026-08-16.md:258` notes the `-3.0/+8.0` and `-2.0/+3.0` noise constants are "ATR-inconsistent … Left unchanged deliberately (deploy-test the scalp first); follow-up is a data-gated retune." The replay optimum was tuned for a LONGER-hold strategy, not the 15m ATR scalp.

### Mainnet-data → testnet-execution mismatch (confirmed)
- Every signal product (ranking, signal-lab, net-flow, cost/liquidation heatmap) is computed exclusively from **MAINNET** Hyperliquid data and fed unchanged into a trader executing on **TESTNET**. No code adapts mainnet prices/liquidity/netflow/heatmap to testnet.
- Signal service: `service/signal/service.go:206-228` `Ingest` → `hyperliquid.GetMarketSnapshot` → `provider/hyperliquid/coins.go:18` `hyperliquidInfoURL = "https://api.hyperliquid.xyz/info"` (mainnet, hardcoded); `service/signal/config.go:82` `HLWSURL = "wss://api.hyperliquid.xyz/ws"` (mainnet WS). No testnet override.
- Engine: `kernel/engine.go:199` builds vergex client → `SIGNAL_SERVICE_BASE_URL` (`docker-compose` → `nofx-signal-service:8480`); `kernel/engine.go:731` `chain = vergex.QueryChain(chain)` → `DefaultChain="mainnet"` (`provider/vergex/client.go:346`); `store/strategy.go:297-298` `VergexChain` default `"hyperliquid"`.
- Execution: `manager/trader_manager.go:686` `HyperliquidTestnet: exchangeCfg.Testnet` → `trader/hyperliquid/trader.go:140-141` → `hyperliquid.TestnetAPIURL` (`provider/hyperliquid/kline.go:16`).
- Market data (ATR14, entry price, klines) used by the AI also comes from mainnet (`market/data_klines.go:116` → `provider/hyperliquid/kline.go:31` `NewClient()` = mainnet; crypto via CoinAnk/Binance mainnet). So SL/TP are derived from MAINNET ATR14 but placed on TESTNET positions.

## Investigator Findings

### Verdict: H1 — trade-throttle noise-band blocks AI loss-cuts — **CONFIRMED (mechanism + headline case)**

**The noise band sits exactly on the ATR scalp's profit/loss zone.**
- Throttle constants (`trader/auto_trader_throttle.go:26-34`): `autopilotMinHoldDuration=90m`, `autopilotNoiseCloseHoldDuration=3h`, `earlyCloseStopLossBypassPct=-3.0`, `earlyCloseTakeProfitBypassPct=8.0`, `noiseCloseLossFloorPct=-2.0`, `noiseCloseProfitCeilingPct=3.0`, `autopilotReentryCooldown=4h`.
- Scalp SL/TP zone: `docs/plans/scalp-15m-atr-tpsl-2026-08-16.md` ("Resolved": **stop = 1.5×ATR / target = 2×ATR**; ATR14 examples in the same doc: KAITO 0.97%, HEMI 4.53%, ACE 5.94%). For a mid-volatility 15m scalp (ATR≈1.5%) the zone is stop≈-2.25% / target≈+3.0%. The noise band `[-2.0%, +3.0%]` therefore **covers nearly the entire scalp P/L zone** — any AI close between -2% and +3% price PnL is throttled until the 3h noise-close hold elapses.
- `closeThrottleReason` (`auto_trader_throttle.go:176`): when `heldFor >= 90m` and `< 3h`, it blocks unless `pnlPct <= -2.0` OR `pnlPct >= +3.0` (L207-215). A loss-cut at -0.86% (inside the band) is blocked → the exact "inside noise band -2.0%..3.0%, wait ~20m" message for XAI.
- **Bypass is wider than the scalp's own SL.** The early-close SL bypass `earlyCloseStopLossBypassPct = -3.0` (L31, applied L222-224) is *wider* than the scalp's own exchange SL (~1.5×ATR ≈ -2.25%). So an AI loss-cut anywhere between -0.86% and -3.0% is blocked before the 90m min-hold too — small losers are never cut by the AI and bleed to the real SL.

**Candidate check (b): throttle gates ONLY AI decision closes, not risk-layer/exchange closes.**
- `tradeThrottleReason` / `closeThrottleReason` are invoked only inside the AI decision-execution loop — `trader/auto_trader_loop.go:331` (`if reason := at.tradeThrottleReason(d, ctx, ...)`), iterating `sortedDecisions` from the AI. It is never called by the position-monitor path.
- SL/TP are placed as **exchange stop/take-profit orders** (`SetStopLoss`/`SetTakeProfit`, re-placed by `moveTrailingStopLoss` `trader/auto_trader_orders.go:129`; ATR-derived via `kernel.ATRStopTarget` `auto_trader_orders.go:44`, `auto_trader_risk.go:241`) and trigger on the exchange, **outside** the throttle. Drawdown/risk auto-closes (`auto_trader_risk.go:160+`) and trailing stops also run in the monitor tick, not through `closeThrottleReason`.
- **Asymmetry:** the throttle can delay the AI's small loss-cut but *cannot* stop the exchange SL. Small losers the AI wants to cut get pushed to the full SL.

**H1 constants tuned for a longer-hold strategy (confirmed):**
- Origin/current values are replay-validated for a longer-hold regime: `574ddfb1` "revert: drop exit-gate configurability, hardcode the replay-validated values … (4154 cycles)" and `39eac5ac` ("hold for big moves … min-hold 60m→4h, noise-close 90m→8h"). `trader/auto_trader_throttle.go:21-23` doc comment states these exit gates "validated by decision replay (2026-07-26, 4154 cycles …)" — i.e. tuned to a long-hold strategy, not the 15m scalp.
- `docs/plans/scalp-15m-atr-tpsl-2026-08-16.md` (the "ATR-inconsistent" bullet) explicitly flags: `earlyCloseStopLossBypassPct=-3.0`/`earlyCloseTakeProfitBypassPct=8.0`, `noiseCloseLossFloorPct=-2.0`/`noiseCloseProfitCeilingPct=3.0` "remain fixed-% while stops are now ATR-scaled and targets can be +3%-ish. Left unchanged deliberately … follow-up is a data-gated retune."

### Verdict: H2 — mainnet signals executed on testnet — **CONFIRMED (systemic, prior-research split re-verified)**

- **Execution is testnet:** `trader/hyperliquid/trader.go:138-141` selects `hyperliquid.TestnetAPIURL` (`provider/hyperliquid/kline.go:16`) when `testnet` is set (`manager/trader_manager.go:686` `HyperliquidTestnet: exchangeCfg.Testnet`).
- **Market data (ATR14, klines) is mainnet:** `market.GetWithTimeframes` → `provider/hyperliquid.NewClient()` (`kline.go:54-56`) defaults to `apiURL: MainnetAPIURL` (kline.go:56; mainnet is the *default*, testnet requires an explicit `NewTestnetClient` L66). `fetchATR14` (`trader/auto_trader_orders.go:102-110`) and the ATR eligibility/sizing paths (`auto_trader_risk.go:173,241`, `auto_trader_force.go:121`) all read this mainnet source.
- **Signals are mainnet:** `service/signal/service.go:206-228` → `provider/hyperliquid/coins.go:18` (`hyperliquidInfoURL = "https://api.hyperliquid.xyz/info"`, mainnet, hardcoded); `service/signal/config.go:82` mainnet WS; engine consumes it with `chain = mainnet` (`kernel/engine.go:199,731`).
- **Candidate check (d) `filterTestnetTradability`:** the tradeability filter only checks symbol existence and mark price > 0 — it does **not** verify testnet order-book depth, spread, or resting-order availability. Hence CASHCAT's repeated "Order could not immediately match against any resting orders" (thin testnet book) passes the filter and fails at fill time.

### Weight: H1 vs H2

- **H1 is the dominant driver of the *close-side* losses and the headline XAI case (~60-70%).** It fully explains the -0.86% → -$8.49 conversion with the literal throttle message, the structurally negative close pattern (winners capped near the +3% ceiling/target, losers stretched to the full SL because the small AI cuts are blocked), and the throttle's asymmetry (it cannot stop the exchange SL). It is a per-close, deterministic mechanism.
- **H2 is a systemic amplifier and the dominant driver of the *entry-side* failures (~30-40%).** It misprices every entry (signal/ATR from mainnet, fill on testnet), and directly explains CASHCAT (testnet no-liquidity) and HMSTR 0m-hold instant SL (entry filled at a testnet price already past the mainnet-derived entry/SL, or mainnet ATR-derived SL tighter than testnet volatility). It does not by itself explain why an AI loss-cut at -0.86% was *blocked* — that is pure H1.
- Net: H1 explains *how* documented small losers become full SLs; H2 explains *why entries/SL levels are wrong to begin with* and the fill failures. Both must be fixed; H1 is the immediate loss mechanism, H2 the root-level correctness flaw.

### Mechanism: -0.86% loser → full -$8.49 SL (XAI short)

1. Position opened with an **exchange SL** at ~1.5×ATR below entry (from mainnet ATR14) and an AI-managed TP target.
2. Price moves to -0.86%; AI issues `close_short`.
3. `closeThrottleReason` blocks it (`auto_trader_throttle.go:176,207-215`): -0.86% is inside the noise band `[-2%,+3%]`, held ≈2h40m (≥90m min-hold, <3h noise-hold) → "wait ~20m". The -3.0% SL bypass (L31, L222-224) does **not** apply because -0.86% > -3.0%.
4. Throttle delays the AI exit; price keeps moving against the position.
5. 12 min later the price reaches the exchange SL (~1.5×ATR ≈ -2.25% ≈ **-$8.49**). The SL is an exchange order, **not** an AI decision, so it is **not** gated by `closeThrottleReason` — it fires regardless.
6. Net: the intended -0.86% loss-cut was deferred to the full -2.25%/-$8.49 SL, ~2.6× larger, purely because the throttle (tuned for a long-hold strategy) sat on the ATR scalp's P/L zone and the bypass was wider than the scalp's own SL.

## Investigation Log

### Phase 1.5 — git archaeology (explore agents)
**Hypothesis:** the throttle noise-band was tuned for a different (longer-hold) strategy than the current 15m scalp. **Finding:** confirmed — noise-band regime originated in `39eac5ac` (Jul 21, "death by small-move grinding"), current constants hardcoded from a decision replay in `574ddfb1` (Jul 26, 4154 cycles); `docs/plans/scalp-15m-atr-tpsl-2026-08-16.md:258` explicitly flags them "ATR-inconsistent". **Conclusion:** confirmed.

### Phase 1.5 — signal source (explore agent)
**Hypothesis:** signals come from mainnet while execution is testnet. **Finding:** confirmed — signal service ingests mainnet Hyperliquid (REST+WS hardcoded), engine consumes with chain=mainnet, execution on TestnetAPIURL; no code adapts mainnet data to testnet. **Conclusion:** confirmed.

### Phase 3 — pair investigator (main line)
**Hypothesis:** H1 (throttle blocks loss-cuts) and H2 (mainnet/testnet mismatch) are the loss drivers. **Finding:** both confirmed; H1 dominant (~60-70%), H2 systemic amplifier (~30-40%); throttled close is AI-only while exchange SL bypasses the throttle. **Conclusion:** confirmed.

### Phase 4 — oracle synthesis
**Hypothesis:** what is the safest recommendation ordering. **Finding:** loss-side fail-open first (network-independent), then network alignment, then full ATR retune; testnet is plumbing-only, not a strategy-efficacy signal. **Conclusion:** confirmed.

## Root Cause

Two independent flaws compound. Neither the drawdown close nor the trailing stop (already implemented and ATR-relative in `trader/auto_trader_risk.go`) is the cause.

### H1 — throttle exit gates block AI loss-cuts (dominant, close-side)
The exit gates in `trader/auto_trader_throttle.go` are hardcoded to a **longer-hold** strategy (commit `574ddfb1`, "4154 cycles" replay) and sit exactly on the 15m ATR scalp's profit/loss zone:
- `noiseCloseLossFloorPct = -2.0` / `noiseCloseProfitCeilingPct = +3.0` (L33-34) and `autopilotNoiseCloseHoldDuration = 3h` (L27). The scalp zone is stop ≈ 1.5×ATR ≈ -2.25%, target ≈ 2×ATR ≈ +3%. So the band `[-2%,+3%]` covers ~the whole scalp P/L range: any AI close between -2% and +3% is throttled.
- `earlyCloseStopLossBypassPct = -3.0` (L31) is **wider** than the scalp's own exchange SL (≈ -2.25%). `closeThrottleReason` (L176) only lets a loss through at `pnlPct <= -3.0` (L222-224), so the AI can never cut a loss between -0.86% and -3.0% — it always bleeds to the exchange SL.
- The throttle gates **only AI decision closes** (`trader/auto_trader_loop.go:331`); the exchange SL/TP (`SetStopLoss`/`SetTakeProfit`, `trader/auto_trader_orders.go:129`) and the drawdown/trailing monitor bypass it. Asymmetry: throttle can delay the AI's loss-cut but cannot stop the SL. Confirmed by the XAI short: AI `close_short` at -0.86% → blocked ("inside noise band -2.0%..3.0%, wait ~20m") → exchange SL fired 12 min later at -$8.49 (~2.6× the intended loss).

### H2 — mainnet data drives testnet execution (systemic, entry-side)
Signals, ATR14, and SL/TP are all computed from **mainnet** Hyperliquid, while orders execute on **testnet**:
- Signal service ingests mainnet: `service/signal/service.go:206-228` → `provider/hyperliquid/coins.go:18` (`hyperliquidInfoURL = "https://api.hyperliquid.xyz/info"`, hardcoded); `service/signal/config.go:82` (`HLWSURL` mainnet WS). Engine consumes it with `chain=mainnet` (`kernel/engine.go:199,731`; `store/strategy.go:297-298`).
- AI market data (ATR14, klines, entry) is mainnet: `market/data_klines.go:116` → `provider/hyperliquid/kline.go:31` `NewClient()` defaults to `MainnetAPIURL`; `trader/auto_trader_orders.go:106` `fetchATR14` reads it.
- Execution is testnet: `trader/hyperliquid/trader.go:140-141` → `TestnetAPIURL` (`provider/hyperliquid/kline.go:16`), via `manager/trader_manager.go:686` `HyperliquidTestnet: exchangeCfg.Testnet`.
- `filterTestnetTradability` (`trader/auto_trader_loop.go:668`) only checks symbol existence + mark>0, not order-book depth/spread — so illiquid names (CASHCAT "no resting orders", HMSTR 0m-hold instant SL) pass the filter and fail at fill time.

Net: H1 explains how documented small losers become full SLs; H2 explains why entry/SL levels are wrong to begin with and why fills fail. Estimated weight ≈ 60-70% H1 / 30-40% H2 for the observed losses.

## Recommendations
1. **H1 loss-side fail-open (immediate, network-independent).** In `trader/auto_trader_throttle.go`, make the loss bypass/floor ATR-relative so an AI loss-cut at/inside the ATR SL passes (e.g. bypass at `pnlPct <= -1.0×ATR%`), closing the "blocked-but-SL-not-fired" window. Keep win-side constants (`noiseCloseProfitCeilingPct`, `autopilotNoiseCloseHoldDuration`, min-hold, 4h reentry, opens caps) unchanged — the fee rationale only ever justified throttling the win-side. Sync `kernel/engine_prompt.go` `vergexHoldRules` so the prompt's loss-cut guidance matches the code.
2. **H2 network alignment (gate).** Route market data + signals to the execution network when testnet: `market/data_klines.go:116` / `provider/hyperliquid/kline.go:31` should use `NewTestnetClient()` (already exists at `kline.go:66`) when `HyperliquidTestnet`; `service/signal/config.go:82` (`HLWSURL`) and `provider/hyperliquid/coins.go:18` need a testnet switch (or gate mainnet signals when executing testnet). Strengthen `filterTestnetTradability` (`trader/auto_trader_loop.go:668`) to require minimum resting depth/spread, and align `auto_trader_force.go` forced-open ATR.
3. **H1 ATR-relative retune of the whole band (after H2).** Only after signals/ATR/SL are self-consistent (same network) can the band be data-gated against a trustworthy ATR. Do not retune the win-side ceiling against mainnet-derived ATR while executing testnet.

## Preventive Measures
- Add a runtime assertion that data-network == execution-network (refuse to start on a mismatch) so the H2 class of bug cannot silently recur.
- Keep throttle exit gates ATR-driven, never hardcoded fixed-% bands that can out-widen a strategy's own SL.
- Treat testnet as integration/plumbing validation only; require self-consistent networks before reporting any performance/win-rate metric.
