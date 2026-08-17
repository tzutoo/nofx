package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"strings"
	"time"
)

const (
	// The monitor arms once the underlying PRICE has moved ~1×ATR in the
	// position's favor (ATR-relative, per-coin via drawdownArmPct), then closes
	// if the position gives back 40% of its peak profit. The old flat +5% arm
	// was unreachable for ATR scalps (TP ≈ +2–3%).
	drawdownCloseGivebackPct = 40.0
	// Fixed fallback arming % when ATR is unavailable.
	drawdownArmFallbackPct = 1.5

	// Drawdown size-trim (#2): as equity falls from its account peak, shrink the
	// position-value multiplier so a losing streak risks progressively less.
	drawdownTrimPct        = 15.0 // at/above 15% below peak -> 0.5x
	drawdownTrimDeepPct    = 30.0 // at/above 30% below peak -> 0.25x
	drawdownTrimScalar     = 0.50
	drawdownTrimDeepScalar = 0.25
)

// shouldDrawdownClose reports whether the profit-protection close should fire.
// pricePnLPct is the price-basis move in the position's favor; armPct is the
// ATR-relative arming threshold; drawdownPct is the relative giveback from the
// position's peak profit.
func shouldDrawdownClose(pricePnLPct, armPct, drawdownPct float64) bool {
	return pricePnLPct > armPct && drawdownPct >= drawdownCloseGivebackPct
}

// drawdownArmPct returns the ATR-relative arming threshold (1×ATR14 as % of
// entry, floored at 1%) for a position, falling back to a fixed 1.5% when ATR
// or entry is unavailable.
func (at *AutoTrader) drawdownArmPct(entryPrice, atr14 float64) float64 {
	if entryPrice > 0 && atr14 > 0 {
		pct := atr14 / entryPrice * 100
		if pct >= 1.0 {
			return pct
		}
	}
	return drawdownArmFallbackPct
}

// startDrawdownMonitor starts drawdown monitoring
func (at *AutoTrader) startDrawdownMonitor() {
	at.monitorWg.Add(1)
	go func() {
		defer at.monitorWg.Done()

		ticker := time.NewTicker(1 * time.Minute) // Check every minute
		defer ticker.Stop()

		logger.Info("📊 Started position drawdown monitoring (check every minute)")

		for {
			select {
			case <-ticker.C:
				positions, err := at.trader.GetPositions()
				if err != nil {
					logger.Infof("❌ Drawdown monitoring: failed to get positions: %v", err)
					continue
				}
				atrCache := make(map[string]float64)
				closed := at.checkPositionDrawdown(positions, atrCache)
				at.checkTrailingStops(positions, atrCache, closed)
			case <-at.stopMonitorCh:
				logger.Info("⏹ Stopped position drawdown monitoring")
				return
			}
		}
	}()
}

// checkPositionDrawdown checks position drawdown situation. It returns the set
// of positions (symbol_side keys) closed this tick so the trail loop skips them.
func (at *AutoTrader) checkPositionDrawdown(positions []map[string]interface{}, atrCache map[string]float64) map[string]bool {
	closed := make(map[string]bool)
	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // Short position quantity is negative, convert to positive
		}

		// Guard: skip if entry price is zero (prevents division by zero panic)
		if entryPrice <= 0 {
			logger.Warnf("⚠️ Drawdown monitoring: %s %s has zero entry price, skipping", symbol, side)
			continue
		}

		// Calculate current P&L percentage
		leverage := 10 // Default value
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}

		// Price-basis move drives the close decision so the trigger point does
		// not tighten as leverage grows; the margin-basis (leveraged) value is
		// only kept for the peak cache shown alongside margin-based PnL% in
		// prompts.
		var pricePnLPct float64
		if side == "long" {
			pricePnLPct = ((markPrice - entryPrice) / entryPrice) * 100
		} else {
			pricePnLPct = ((entryPrice - markPrice) / entryPrice) * 100
		}
		currentPnLPct := pricePnLPct * float64(leverage)

		// Construct unique position identifier (distinguish long/short)
		posKey := positionKey(symbol, side)

		// Get historical peak profit for this position
		at.peakPnLCacheMutex.RLock()
		peakPnLPct, exists := at.peakPnLCache[posKey]
		at.peakPnLCacheMutex.RUnlock()

		if !exists {
			// If no historical peak record, use current P&L as initial value
			peakPnLPct = currentPnLPct
			at.UpdatePeakPnL(symbol, side, currentPnLPct)
		} else {
			// Update peak cache
			at.UpdatePeakPnL(symbol, side, currentPnLPct)
		}

		// Calculate drawdown (magnitude of decline from peak)
		var drawdownPct float64
		if peakPnLPct > 0 && currentPnLPct < peakPnLPct {
			drawdownPct = ((peakPnLPct - currentPnLPct) / peakPnLPct) * 100
		}

		armPct := at.drawdownArmPct(entryPrice, at.atrForSymbol(atrCache, symbol))

		// Check close position condition: price move > armPct and drawdown >= 40%
		if shouldDrawdownClose(pricePnLPct, armPct, drawdownPct) {
			logger.Infof("🚨 Drawdown close position condition triggered: %s %s | Price move: %.2f%% | Current profit: %.2f%% | Peak profit: %.2f%% | Drawdown: %.2f%%",
				symbol, side, pricePnLPct, currentPnLPct, peakPnLPct, drawdownPct)

			// Execute close position
			if err := at.emergencyClosePosition(symbol, side); err != nil {
				logger.Infof("❌ Drawdown close position failed (%s %s): %v", symbol, side, err)
			} else {
				logger.Infof("✅ Drawdown close position succeeded: %s %s", symbol, side)
				// Clear cache for this position after closing
				at.ClearPeakPnLCache(symbol, side)
				closed[posKey] = true
			}
		} else if pricePnLPct > armPct {
			// Record situations close to close position condition (for debugging)
			logger.Infof("📊 Drawdown monitoring: %s %s | Price move: %.2f%% | Profit: %.2f%% | Peak: %.2f%% | Drawdown: %.2f%%",
				symbol, side, pricePnLPct, currentPnLPct, peakPnLPct, drawdownPct)
		}
	}
	return closed
}

// atrForSymbol returns the 15m ATR14 for a symbol, cached per tick in atrCache.
func (at *AutoTrader) atrForSymbol(atrCache map[string]float64, symbol string) float64 {
	if v, ok := atrCache[symbol]; ok {
		return v
	}
	v := at.fetchATR14(symbol)
	atrCache[symbol] = v
	return v
}

// checkTrailingStops ratchets the exchange stop-loss up (break-even at +1×ATR,
// then 1×ATR behind peak) for winning positions. It never touches TP.
func (at *AutoTrader) checkTrailingStops(positions []map[string]interface{}, atrCache map[string]float64, closed map[string]bool) {
	if len(positions) == 0 {
		return
	}
	seen := make(map[string]bool)
	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := strings.ToLower(pos["side"].(string))
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		if entryPrice <= 0 {
			continue
		}
		isLong := side == "long"
		posKey := positionKey(symbol, side)
		seen[posKey] = true
		if closed[posKey] {
			continue // closed by the drawdown check this tick
		}

		// Peak-price update (monotonic; first-seen = mark).
		at.peakPriceMu.Lock()
		if at.peakPrice == nil {
			at.peakPrice = make(map[string]float64)
		}
		peak, exists := at.peakPrice[posKey]
		if !exists {
			peak = markPrice
		} else if isLong {
			if markPrice > peak {
				peak = markPrice
			}
		} else {
			if markPrice < peak {
				peak = markPrice
			}
		}
		at.peakPrice[posKey] = peak
		at.peakPriceMu.Unlock()

		// ATR (fail-closed: no trail action without volatility data).
		atr14 := at.atrForSymbol(atrCache, symbol)
		if atr14 <= 0 {
			continue
		}

		// Seed trailState if absent (restart fallback).
		at.trailStateMu.Lock()
		if at.trailState == nil {
			at.trailState = make(map[string]trailingStopState)
		}
		state, hasState := at.trailState[posKey]
		if !hasState {
			if stop, tp, ok := kernel.ATRStopTarget(entryPrice, atr14, at.atrStopMultiplier(), at.atrTargetMultiplier(), isLong); ok {
				state = trailingStopState{stop: stop, tp: tp}
				at.trailState[posKey] = state
			} else {
				at.trailStateMu.Unlock()
				continue
			}
		}
		at.trailStateMu.Unlock()

		// Trail candidate (arm at +1×ATR, trail 1×ATR behind peak).
		trail, ok := kernel.TrailStopTarget(entryPrice, peak, atr14, at.atrTrailMultiplier(), isLong)
		if !ok {
			continue // not yet armed
		}

		// Ratchet: only tighten toward the peak.
		var newStop float64
		if isLong {
			if trail <= state.stop {
				continue
			}
			newStop = trail
		} else {
			if trail >= state.stop {
				continue
			}
			newStop = trail
		}

		// Epsilon gate: avoid churn from floating-point noise.
		if math.Abs((newStop-state.stop)/entryPrice) <= 1e-4 {
			continue
		}

		// Move (cancel + re-place SL + re-place TP).
		if err := at.moveTrailingStopLoss(symbol, isLong, quantity, newStop, state.tp); err != nil {
			logger.Infof("❌ Trailing stop move failed (%s %s): %v", symbol, side, err)
			continue // state not updated -> retry next tick
		}
		at.trailStateMu.Lock()
		if s, ok := at.trailState[posKey]; ok {
			s.stop = newStop
			at.trailState[posKey] = s
		}
		at.trailStateMu.Unlock()
		logger.Infof("🎯 Trailing stop ratcheted %s %s: %.4f -> %.4f (peak %.4f, ATR %.4f)", symbol, side, state.stop, newStop, peak, atr14)
	}

	// GC: prune keys not in the current position set (every close path).
	at.peakPriceMu.Lock()
	for k := range at.peakPrice {
		if !seen[k] {
			delete(at.peakPrice, k)
		}
	}
	at.peakPriceMu.Unlock()
	at.trailStateMu.Lock()
	for k := range at.trailState {
		if !seen[k] {
			delete(at.trailState, k)
		}
	}
	at.trailStateMu.Unlock()
}

// emergencyClosePosition emergency close position function
func (at *AutoTrader) emergencyClosePosition(symbol, side string) error {
	switch side {
	case "long":
		order, err := at.trader.CloseLong(symbol, 0) // 0 = close all
		if err != nil {
			return err
		}
		logger.Infof("✅ Emergency close long position succeeded, order ID: %v", order["orderId"])
	case "short":
		order, err := at.trader.CloseShort(symbol, 0) // 0 = close all
		if err != nil {
			return err
		}
		logger.Infof("✅ Emergency close short position succeeded, order ID: %v", order["orderId"])
	default:
		return fmt.Errorf("unknown position direction: %s", side)
	}

	return nil
}

// GetPeakPnLCache gets peak profit cache
func (at *AutoTrader) GetPeakPnLCache() map[string]float64 {
	at.peakPnLCacheMutex.RLock()
	defer at.peakPnLCacheMutex.RUnlock()

	// Return a copy of the cache
	cache := make(map[string]float64)
	for k, v := range at.peakPnLCache {
		cache[k] = v
	}
	return cache
}

// UpdatePeakPnL updates peak profit cache
func (at *AutoTrader) UpdatePeakPnL(symbol, side string, currentPnLPct float64) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	posKey := symbol + "_" + side
	if peak, exists := at.peakPnLCache[posKey]; exists {
		// Update peak (if long, take larger value; if short, currentPnLPct is negative, also compare)
		if currentPnLPct > peak {
			at.peakPnLCache[posKey] = currentPnLPct
		}
	} else {
		// First time recording
		at.peakPnLCache[posKey] = currentPnLPct
	}
}

// ClearPeakPnLCache clears peak cache for specified position
func (at *AutoTrader) ClearPeakPnLCache(symbol, side string) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	posKey := symbol + "_" + side
	delete(at.peakPnLCache, posKey)
}

// ============================================================================
// Risk Control Helpers
// ============================================================================

// isBTCETH checks if a symbol is BTC or ETH
func isBTCETH(symbol string) bool {
	symbol = strings.ToUpper(symbol)
	return strings.HasPrefix(symbol, "BTC") || strings.HasPrefix(symbol, "ETH")
}

// isMajorAsset returns true for assets that should use the BTC/ETH higher
// position-value tier rather than the altcoin (1x equity) tier. This covers
// BTC/ETH crypto perps AND Hyperliquid XYZ assets (US equities, commodities,
// forex) — none of which are "altcoins" and all of which deserve the higher
// per-position cap so the AI can actually take meaningful positions.
func isMajorAsset(symbol string) bool {
	if isBTCETH(symbol) {
		return true
	}
	return market.IsXyzDexAsset(symbol)
}

// riskPerTradePct returns the max %% of equity a single position may lose at its
// stop (from StrategyConfig, default 3.0).
func (at *AutoTrader) riskPerTradePct() float64 {
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.RiskPerTradePct > 0 {
		return at.config.StrategyConfig.RiskControl.RiskPerTradePct
	}
	return 3.0
}

// correlationThreshold returns the same-theme/correlation block threshold from
// StrategyConfig (default 0.7). A negative value disables the block; 0 means
// "unset -> use the 0.7 default" (so existing strategies get the protection);
// a positive value is used as-is.
func (at *AutoTrader) correlationThreshold() float64 {
	if at.config.StrategyConfig == nil {
		return 0.7
	}
	t := at.config.StrategyConfig.RiskControl.CorrelationBlockThreshold
	switch {
	case t < 0:
		return 0 // disabled
	case t == 0:
		return 0.7 // unset -> default
	default:
		return t
	}
}

// correlationBlockReason reports why an open should be blocked for correlation:
// the candidate moves in lockstep (>= threshold) with an already-held position
// that will remain open this cycle. It fails open: if correlation data is
// unavailable or sparse, the open is allowed. Works for long and short (the
// block targets same-direction co-movement, which is the concentration risk).
// closing is the set of symbols being closed this cycle (excluded, since they
// will not remain open).
func (at *AutoTrader) correlationBlockReason(d kernel.Decision, ctx *kernel.Context, closing map[string]bool) string {
	if ctx == nil || !isOpenAction(d.Action) {
		return ""
	}
	threshold := at.correlationThreshold()
	if threshold <= 0 {
		return ""
	}
	cand := normalizedDecisionSymbol(d.Symbol)
	if cand == "" {
		return ""
	}
	const (
		timeframe = "1d"
		lookback  = 30 * 24 * time.Hour
	)
	for _, pos := range ctx.Positions {
		sym := normalizedDecisionSymbol(pos.Symbol)
		if sym == "" || sym == cand || closing[sym] {
			continue
		}
		corr, ok, err := market.SymbolPairCorrelation(cand, sym, timeframe, lookback)
		if err != nil || !ok {
			continue // fail-open on insufficient/error data
		}
		if corr >= threshold {
			return fmt.Sprintf("correlation block: %s is %.0f%% correlated with held %s (threshold %.0f%%)", cand, corr*100, pos.Symbol, threshold*100)
		}
	}
	return ""
}

// riskScaleForDrawdown returns a size multiplier (1.0, 0.5, 0.25) based on how
// far current equity is below the tracked account peak, so a losing streak
// progressively shrinks position exposure. Direction-agnostic (whole account).
func (at *AutoTrader) riskScaleForDrawdown(equity float64) float64 {
	at.peakPnLCacheMutex.RLock()
	peak := at.peakEquity
	at.peakPnLCacheMutex.RUnlock()
	if peak <= 0 {
		return 1.0
	}
	dd := (peak - equity) / peak
	if dd <= 0 {
		return 1.0
	}
	switch {
	case dd > drawdownTrimDeepPct/100.0:
		return drawdownTrimDeepScalar
	case dd > drawdownTrimPct/100.0:
		return drawdownTrimScalar
	default:
		return 1.0
	}
}

// enforcePositionValueRatio checks and enforces position value ratio limits (CODE ENFORCED)
// Returns the adjusted position size (capped if necessary) and whether the position was capped
// positionSizeUSD: the original position size in USD
// equity: the account equity
// symbol: the trading symbol
func (at *AutoTrader) enforcePositionValueRatio(positionSizeUSD float64, equity float64, symbol string) (float64, bool) {
	if at.config.StrategyConfig == nil {
		return positionSizeUSD, false
	}

	riskControl := at.config.StrategyConfig.RiskControl

	// Get the appropriate position value ratio limit. BTC/ETH AND Hyperliquid
	// XYZ assets (US stocks etc.) use the higher tier; pure altcoins use the
	// lower tier.
	var maxPositionValueRatio float64
	if isMajorAsset(symbol) {
		maxPositionValueRatio = riskControl.BTCETHMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 5.0 // Default: 5x for BTC/ETH and XYZ assets
		}
	} else {
		maxPositionValueRatio = riskControl.AltcoinMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 1.0 // Default: 1x for altcoins
		}
	}

	// Calculate max allowed position value = equity × ratio (× drawdown size-trim)
	maxPositionValue := equity * maxPositionValueRatio * at.riskScaleForDrawdown(equity)

	// Check if position size exceeds limit
	if positionSizeUSD > maxPositionValue {
		logger.Infof("  ⚠️ [RISK CONTROL] Position %.2f USDT exceeds limit (equity %.2f × %.1fx = %.2f USDT max for %s), capping",
			positionSizeUSD, equity, maxPositionValueRatio, maxPositionValue, symbol)
		return maxPositionValue, true
	}

	return positionSizeUSD, false
}

func (at *AutoTrader) applyAutopilotFullSizeOpen(decision *kernel.Decision, equity float64) {
	if at == nil || decision == nil || at.config.StrategyConfig == nil || equity <= 0 {
		return
	}

	cfg := at.config.StrategyConfig
	if cfg.CoinSource.SourceType != "vergex_signal" {
		return
	}

	riskControl := cfg.RiskControl
	leverage := riskControl.AltcoinMaxLeverage
	positionValueRatio := riskControl.AltcoinMaxPositionValueRatio
	if isMajorAsset(decision.Symbol) {
		leverage = riskControl.BTCETHMaxLeverage
		positionValueRatio = riskControl.BTCETHMaxPositionValueRatio
	}
	if leverage < store.MinLeverage {
		leverage = store.MinLeverage
	}
	if leverage > store.MaxAltLeverage {
		leverage = store.MaxAltLeverage
	}
	if positionValueRatio <= 0 {
		positionValueRatio = 1.0
	}

	fullPositionSize := equity * positionValueRatio
	if fullPositionSize <= 0 {
		return
	}

	// #1 risk cap: bound notional so a stop-out loses at most RiskPerTradePct%% of
	// equity (long and short both covered via the price-move stop distance).
	// This gate is gated on SL/TP being present because the force-time call (in
	// ensureLongShortCoverage) has no stop yet; the execution-time re-size in
	// executeOpen* now fills the ATR stop BEFORE this runs, so forced opens do
	// get the risk cap at execution.
	if decision.StopLoss > 0 && decision.TakeProfit > 0 {
		var entry float64
		if decision.Action == "open_long" {
			entry = decision.StopLoss + (decision.TakeProfit-decision.StopLoss)*0.2
		} else {
			entry = decision.StopLoss - (decision.StopLoss-decision.TakeProfit)*0.2
		}
		if capped := kernel.RiskCappedPositionSize(equity, entry, decision.StopLoss, decision.Action == "open_long", at.riskPerTradePct(), fullPositionSize); capped < fullPositionSize {
			fullPositionSize = capped
		}
	}

	if decision.Leverage != leverage || decision.PositionSizeUSD != fullPositionSize {
		logger.Infof("  📏 [AUTOPILOT] Full-size open enforced for %s: leverage %dx → %dx, notional %.2f → %.2f USDT",
			decision.Symbol, decision.Leverage, leverage, decision.PositionSizeUSD, fullPositionSize)
	}
	decision.Leverage = leverage
	decision.PositionSizeUSD = fullPositionSize
}

// enforceMinPositionSize checks minimum position size (CODE ENFORCED)
func (at *AutoTrader) enforceMinPositionSize(positionSizeUSD float64) error {
	if at.config.StrategyConfig == nil {
		return nil
	}

	minSize := at.config.StrategyConfig.RiskControl.MinPositionSize
	if minSize <= 0 {
		minSize = 12 // Default: 12 USDT
	}

	if positionSizeUSD < minSize {
		return fmt.Errorf("❌ [RISK CONTROL] Position %.2f USDT below minimum (%.2f USDT)", positionSizeUSD, minSize)
	}
	return nil
}

// enforceMaxPositions checks maximum positions count (CODE ENFORCED)
func (at *AutoTrader) enforceMaxPositions(currentPositionCount int) error {
	if at.config.StrategyConfig == nil {
		return nil
	}

	maxPositions := at.config.StrategyConfig.RiskControl.MaxPositions
	if maxPositions <= 0 {
		maxPositions = 3 // Default: 3 positions
	}

	if currentPositionCount >= maxPositions {
		return fmt.Errorf("❌ [RISK CONTROL] Already at max positions (%d/%d)", currentPositionCount, maxPositions)
	}
	return nil
}

// getSideFromAction converts order action to side (BUY/SELL)
func getSideFromAction(action string) string {
	switch action {
	case "open_long", "close_short":
		return "BUY"
	case "open_short", "close_long":
		return "SELL"
	default:
		return "BUY"
	}
}
