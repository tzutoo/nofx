package trader

import (
	"fmt"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"strings"
	"time"
)

const (
	// marginOverheadFactor and takerFeeRate approximate the total funds an
	// exchange reserves when opening a position:
	// totalRequired ≈ positionSize/leverage + positionSize*takerFeeRate + positionSize/leverage*1%
	//              = positionSize * (marginOverheadFactor/leverage + takerFeeRate)
	marginOverheadFactor = 1.01
	takerFeeRate         = 0.001

	// positionSizeSafetyFactor leaves a buffer below the maximum affordable
	// position size so a price move between sizing and execution cannot
	// trigger an insufficient-margin rejection.
	positionSizeSafetyFactor = 0.98

	// defaultStopLossPct / defaultTakeProfitPct are the fallback SL/TP levels
	// (as a fraction of the entry price) applied when a decision omits them,
	// matching the vergexHoldRules prompt guidance (~-3% stop, ~+8% target).
	defaultStopLossPct   = 0.03
	defaultTakeProfitPct = 0.08

	// minATRFraction is the minimum ATR14 (as a fraction of price) below which
	// ATR is treated as invalid. Testnet klines for low-liquidity coins are often
	// flat (near-zero range), yielding a garbage ~0 ATR that makes the trailing
	// stop arm instantly and ratchet the SL to the current price. Below this
	// floor we return 0 so open-time SL/TP fall back to the fixed default and
	// the trail skips (fail-closed) instead of churning.
	minATRFraction = 0.005 // 0.5% of price
)

// ensureStopLossTakeProfitDefaults fills BOTH SL/TP from entryPrice whenever
// EITHER is <= 0. Returns an error only when entryPrice is unusable (<= 0) —
// callers abort the open before positioning. Never mutates a fully-specified pair.
func (at *AutoTrader) ensureStopLossTakeProfitDefaults(d *kernel.Decision, entryPrice, atr14 float64) error {
	if d.StopLoss > 0 && d.TakeProfit > 0 {
		return nil
	}
	if entryPrice <= 0 {
		return fmt.Errorf("cannot derive default SL/TP from invalid entry price %.4f for %s", entryPrice, d.Symbol)
	}
	isLong := d.Action != "open_short"
	if stop, target, ok := kernel.ATRStopTarget(entryPrice, atr14, at.atrStopMultiplier(), at.atrTargetMultiplier(), isLong); ok {
		d.StopLoss = stop
		d.TakeProfit = target
		at.logInfof("Filled ATR default SL/TP for %s from entry %.4f (ATR14 %.4f): SL=%.4f TP=%.4f", d.Symbol, entryPrice, atr14, d.StopLoss, d.TakeProfit)
		return nil
	}
	if d.Action == "open_short" {
		d.StopLoss = entryPrice * (1 + defaultStopLossPct)
		d.TakeProfit = entryPrice * (1 - defaultTakeProfitPct)
	} else {
		d.StopLoss = entryPrice * (1 - defaultStopLossPct)
		d.TakeProfit = entryPrice * (1 + defaultTakeProfitPct)
	}
	at.logInfof("Filled fixed default SL/TP for %s from entry %.4f: SL=%.4f TP=%.4f", d.Symbol, entryPrice, d.StopLoss, d.TakeProfit)
	return nil
}

// atrStopMultiplier / atrTargetMultiplier / atrTimeframe read the ATR scalp
// config with 0→default fallback (a legacy strategy has 0 for the new fields).
func (at *AutoTrader) atrStopMultiplier() float64 {
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.ATRStopMultiplier > 0 {
		return at.config.StrategyConfig.RiskControl.ATRStopMultiplier
	}
	return 1.5
}

func (at *AutoTrader) atrTargetMultiplier() float64 {
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.ATRTargetMultiplier > 0 {
		return at.config.StrategyConfig.RiskControl.ATRTargetMultiplier
	}
	return 2.0
}

func (at *AutoTrader) atrTrailMultiplier() float64 {
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.ATRTrailMultiplier > 0 {
		return at.config.StrategyConfig.RiskControl.ATRTrailMultiplier
	}
	return 1.0
}

func (at *AutoTrader) atrTimeframe() string {
	if at.config.StrategyConfig != nil && at.config.StrategyConfig.RiskControl.ATREligibilityTimeframe != "" {
		return at.config.StrategyConfig.RiskControl.ATREligibilityTimeframe
	}
	return "15m"
}

// atrEligibilityCapPct returns the ATR eligibility cap (0 = disabled, matching
// the CorrelationBlockThreshold convention). Legacy strategies have 0 until set.
func (at *AutoTrader) atrEligibilityCapPct() float64 {
	if at.config.StrategyConfig == nil {
		return 0
	}
	return at.config.StrategyConfig.RiskControl.ATREligibilityCapPct
}

// fetchATR14 fetches the 15m ATR14 for a symbol (0 on failure/absent). Reads
// TimeframeData[tf].ATR14 — the top-level market.Data struct has no ATR14 field.
func (at *AutoTrader) fetchATR14(symbol string) float64 {
	tf := at.atrTimeframe()
	data, err := market.GetWithTimeframesForNetwork(symbol, []string{tf}, tf, 30, at.config.HyperliquidTestnet)
	if err != nil || data == nil || data.TimeframeData == nil || data.TimeframeData[tf] == nil {
		return 0
	}
	tfData := data.TimeframeData[tf]
	atr14 := tfData.ATR14
	// Floor: treat a degenerate (flat/sparse) ATR as invalid so the trailing stop
	// does not arm at ~0% and ratchet the SL to the current price, and so
	// open-time SL/TP fall back to the fixed default instead of a ~0-width stop.
	if len(tfData.Klines) > 0 {
		if price := tfData.Klines[len(tfData.Klines)-1].Close; price > 0 && atr14 < minATRFraction*price {
			return 0
		}
	}
	return atr14
}

// positionKey returns the canonical position key (symbol_side, side lowercased).
// The symbol is normalized to its bare base (e.g. "PUMPUSDT", "xyz:PUMP", and
// "PUMP" all become "PUMP") so the open path and the position-monitor path agree
// on the same key regardless of quote-suffix or prefix differences.
func positionKey(symbol, side string) string {
	return universeBaseKey(symbol) + "_" + strings.ToLower(side)
}

// moveTrailingStopLoss re-places the stop-loss (ratcheted up) and re-places the
// take-profit unchanged, because CancelStopOrders cancels coin-wide (Hyperliquid
// cannot distinguish SL from TP). tp is the already-placed TP — never moved.
func (at *AutoTrader) moveTrailingStopLoss(symbol string, isLong bool, quantity, newStop, tp float64) error {
	side := "LONG"
	if !isLong {
		side = "SHORT"
	}
	if err := at.trader.CancelStopOrders(symbol); err != nil {
		return fmt.Errorf("failed to cancel stop orders for %s: %w", symbol, err)
	}
	if err := at.trader.SetStopLoss(symbol, side, quantity, newStop); err != nil {
		return fmt.Errorf("failed to move stop loss for %s to %.4f: %w", symbol, newStop, err)
	}
	if err := at.trader.SetTakeProfit(symbol, side, quantity, tp); err != nil {
		return fmt.Errorf("failed to re-place take profit for %s at %.4f: %w", symbol, tp, err)
	}
	return nil
}

// attachStopLossTakeProfit places both reduce-only trigger orders for an open
// position. Any failure returns immediately (position stays open on the
// exchange — caller records the error). Called only after
// ensureStopLossTakeProfitDefaults, so prices are always > 0.
func (at *AutoTrader) attachStopLossTakeProfit(symbol, side string, quantity, entryPrice, stopLoss, takeProfit float64) error {
	if err := at.trader.SetStopLoss(symbol, side, quantity, stopLoss); err != nil {
		return fmt.Errorf("opened %s but failed to set stop loss at %.4f: %w", symbol, stopLoss, err)
	}
	if err := at.trader.SetTakeProfit(symbol, side, quantity, takeProfit); err != nil {
		return fmt.Errorf("opened %s but failed to set take profit at %.4f: %w", symbol, takeProfit, err)
	}
	// Record the placed SL/TP as the trail's source of truth (open-time seed).
	at.trailStateMu.Lock()
	if at.trailState == nil {
		at.trailState = make(map[string]trailingStopState)
	}
	at.trailState[positionKey(symbol, side)] = trailingStopState{entry: entryPrice, stop: stopLoss, tp: takeProfit}
	at.trailStateMu.Unlock()
	return nil
}

// executeDecisionWithRecord executes AI decision and records detailed information
func (at *AutoTrader) executeDecisionWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	switch decision.Action {
	case "open_long":
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
		return at.executeOpenShortWithRecord(decision, actionRecord)
	case "close_long":
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "close_short":
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "hold", "wait":
		// No execution needed, just record
		return nil
	default:
		return fmt.Errorf("unknown action: %s", decision.Action)
	}
}

// executeOpenLongWithRecord executes open long position and records detailed information
func (at *AutoTrader) executeOpenLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📈 Open long: %s", decision.Symbol)

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// [CODE ENFORCED] Check max positions limit
	if err := at.enforceMaxPositions(len(positions)); err != nil {
		return err
	}

	// Check if there's already a position in the same symbol and direction
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
			return fmt.Errorf("❌ %s already has long position, close it first", decision.Symbol)
		}
	}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// Derive the ATR stop/target BEFORE sizing so the risk cap in
	// applyAutopilotFullSizeOpen runs on a real stop (forced opens carry
	// SL/TP=0 and would otherwise skip the execution risk cap).
	if decision.StopLoss <= 0 || decision.TakeProfit <= 0 {
		atr14 := at.fetchATR14(decision.Symbol)
		if err := at.ensureStopLossTakeProfitDefaults(decision, marketData.CurrentPrice, atr14); err != nil {
			return fmt.Errorf("aborting %s open without SL/TP: %w", decision.Symbol, err)
		}
		actionRecord.StopLoss = decision.StopLoss
		actionRecord.TakeProfit = decision.TakeProfit
	}

	at.applyAutopilotFullSizeOpen(decision, equity)

	// [CODE ENFORCED] Position Value Ratio Check: position_value <= equity × ratio
	adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjustedPositionSize
	}

	// ⚠️ Auto-adjust position size if insufficient margin
	marginFactor := marginOverheadFactor/float64(decision.Leverage) + takerFeeRate
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		adjustedSize := maxAffordablePositionSize * positionSizeSafetyFactor
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// Set margin mode
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.trader.OpenLong(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return fmt.Errorf("failed to open long position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_long", quantity, marketData.CurrentPrice, decision.Leverage, 0)

	// Record position opening time
	posKey := decision.Symbol + "_long"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// Set stop loss and take profit (fatal: never leave an open position unprotected)
	if err := at.attachStopLossTakeProfit(decision.Symbol, "LONG", quantity, marketData.CurrentPrice, decision.StopLoss, decision.TakeProfit); err != nil {
		return err
	}

	return nil
}

// executeOpenShortWithRecord executes open short position and records detailed information
func (at *AutoTrader) executeOpenShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📉 Open short: %s", decision.Symbol)

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// [CODE ENFORCED] Check max positions limit
	if err := at.enforceMaxPositions(len(positions)); err != nil {
		return err
	}

	// Check if there's already a position in the same symbol and direction
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
			return fmt.Errorf("❌ %s already has short position, close it first", decision.Symbol)
		}
	}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// Derive the ATR stop/target BEFORE sizing so the risk cap in
	// applyAutopilotFullSizeOpen runs on a real stop (forced opens carry
	// SL/TP=0 and would otherwise skip the execution risk cap).
	if decision.StopLoss <= 0 || decision.TakeProfit <= 0 {
		atr14 := at.fetchATR14(decision.Symbol)
		if err := at.ensureStopLossTakeProfitDefaults(decision, marketData.CurrentPrice, atr14); err != nil {
			return fmt.Errorf("aborting %s open without SL/TP: %w", decision.Symbol, err)
		}
		actionRecord.StopLoss = decision.StopLoss
		actionRecord.TakeProfit = decision.TakeProfit
	}

	at.applyAutopilotFullSizeOpen(decision, equity)

	// [CODE ENFORCED] Position Value Ratio Check: position_value <= equity × ratio
	adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjustedPositionSize
	}

	// ⚠️ Auto-adjust position size if insufficient margin
	marginFactor := marginOverheadFactor/float64(decision.Leverage) + takerFeeRate
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		adjustedSize := maxAffordablePositionSize * positionSizeSafetyFactor
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// Set margin mode
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.trader.OpenShort(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return fmt.Errorf("failed to open short position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_short", quantity, marketData.CurrentPrice, decision.Leverage, 0)

	// Record position opening time
	posKey := decision.Symbol + "_short"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// Set stop loss and take profit (fatal: never leave an open position unprotected)
	if err := at.attachStopLossTakeProfit(decision.Symbol, "SHORT", quantity, marketData.CurrentPrice, decision.StopLoss, decision.TakeProfit); err != nil {
		return err
	}

	return nil
}

// executeCloseLongWithRecord executes close long position and records detailed information
func (at *AutoTrader) executeCloseLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close long: %s", decision.Symbol)

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}
	actionRecord.Price = marketData.CurrentPrice

	// Normalize symbol for database lookup
	normalizedSymbol := market.Normalize(decision.Symbol)

	// Get entry price and quantity - prioritize local database for accurate quantity
	var entryPrice float64
	var quantity float64

	// First try to get from local database (more accurate for quantity)
	if at.store != nil {
		if openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, normalizedSymbol, "LONG"); err == nil && openPos != nil {
			quantity = openPos.Quantity
			entryPrice = openPos.EntryPrice
			logger.Infof("  📊 Using local position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
		}
	}

	// Fallback to exchange API if local data not found
	if quantity == 0 {
		positions, err := at.trader.GetPositions()
		if err == nil {
			for _, pos := range positions {
				if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
					if ep, ok := pos["entryPrice"].(float64); ok {
						entryPrice = ep
					}
					if amt, ok := pos["positionAmt"].(float64); ok && amt > 0 {
						quantity = amt
					}
					break
				}
			}
		}
		logger.Infof("  📊 Using exchange position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
	}

	// Close position
	order, err := at.trader.CloseLong(decision.Symbol, 0) // 0 = close all
	if err != nil {
		return fmt.Errorf("failed to close long position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_long", quantity, marketData.CurrentPrice, 0, entryPrice)

	logger.Infof("  ✓ Position closed successfully")
	return nil
}

// executeCloseShortWithRecord executes close short position and records detailed information
func (at *AutoTrader) executeCloseShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close short: %s", decision.Symbol)

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return fmt.Errorf("failed to get market data for %s: %w", decision.Symbol, err)
	}
	actionRecord.Price = marketData.CurrentPrice

	// Normalize symbol for database lookup
	normalizedSymbol := market.Normalize(decision.Symbol)

	// Get entry price and quantity - prioritize local database for accurate quantity
	var entryPrice float64
	var quantity float64

	// First try to get from local database (more accurate for quantity)
	if at.store != nil {
		if openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, normalizedSymbol, "SHORT"); err == nil && openPos != nil {
			quantity = openPos.Quantity
			entryPrice = openPos.EntryPrice
			logger.Infof("  📊 Using local position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
		}
	}

	// Fallback to exchange API if local data not found
	if quantity == 0 {
		positions, err := at.trader.GetPositions()
		if err == nil {
			for _, pos := range positions {
				if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
					if ep, ok := pos["entryPrice"].(float64); ok {
						entryPrice = ep
					}
					if amt, ok := pos["positionAmt"].(float64); ok {
						quantity = -amt // positionAmt is negative for short
					}
					break
				}
			}
		}
		logger.Infof("  📊 Using exchange position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
	}

	// Close position
	order, err := at.trader.CloseShort(decision.Symbol, 0) // 0 = close all
	if err != nil {
		return fmt.Errorf("failed to close short position for %s: %w", decision.Symbol, err)
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_short", quantity, marketData.CurrentPrice, 0, entryPrice)

	logger.Infof("  ✓ Position closed successfully")
	return nil
}
