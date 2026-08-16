package kernel

import (
	"fmt"
	"nofx/logger"
	"nofx/market"
)

// ============================================================================
// Decision Validation
// ============================================================================

// RiskCappedPositionSize returns the largest notional (in USD) that risks at
// most riskBudgetPct%% of equity if the stop is hit, clamped to notionalCap.
// priceRisk is the fraction of notional lost on a stop-out measured on PRICE
// move (leverage-independent), so the cap is correct at any leverage and for
// both long and short positions. A non-positive priceRisk (stop on the wrong
// side or degenerate prices) falls back to notionalCap and is left to the
// caller's other validators to reject.
func RiskCappedPositionSize(equity, entryPrice, stopPrice float64, isLong bool, riskBudgetPct, notionalCap float64) float64 {
	if equity <= 0 || entryPrice <= 0 || stopPrice <= 0 {
		return notionalCap
	}
	priceRisk := (entryPrice - stopPrice) / entryPrice // +ve for long
	if !isLong {
		priceRisk = (stopPrice - entryPrice) / entryPrice // +ve for short
	}
	if priceRisk <= 0 {
		return notionalCap
	}
	riskBudgetUSD := (riskBudgetPct / 100.0) * equity
	riskSize := riskBudgetUSD / priceRisk
	if riskSize > notionalCap {
		riskSize = notionalCap
	}
	return riskSize
}

func validateDecisions(decisions []Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64, riskPerTradePct float64) error {
	for i := range decisions {
		if err := validateDecision(&decisions[i], accountEquity, btcEthLeverage, altcoinLeverage, btcEthPosRatio, altcoinPosRatio, riskPerTradePct); err != nil {
			return fmt.Errorf("decision #%d validation failed: %w", i+1, err)
		}
	}
	return nil
}

func validateDecision(d *Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64, riskPerTradePct float64) error {
	validActions := map[string]bool{
		"open_long":   true,
		"open_short":  true,
		"close_long":  true,
		"close_short": true,
		"hold":        true,
		"wait":        true,
	}

	if !validActions[d.Action] {
		return fmt.Errorf("invalid action: %s", d.Action)
	}

	if d.Action == "open_long" || d.Action == "open_short" {
		// Asset tiering for validation:
		//   - BTC/ETH crypto perps use the BTC/ETH tier (typically 5x equity).
		//   - Hyperliquid XYZ assets (US equities, commodities, forex) are
		//     also treated as the higher tier — they are not crypto altcoins
		//     and the user's quick-trade flow shows them at the higher cap,
		//     so the validator must match.
		//   - Everything else is altcoin (1x equity by default).
		maxLeverage := altcoinLeverage
		posRatio := altcoinPosRatio
		maxPositionValue := accountEquity * posRatio
		isMajor := d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" || market.IsXyzDexAsset(d.Symbol)
		if isMajor {
			maxLeverage = btcEthLeverage
			posRatio = btcEthPosRatio
			maxPositionValue = accountEquity * posRatio
		}

		if d.Leverage <= 0 {
			return fmt.Errorf("leverage must be greater than 0: %d", d.Leverage)
		}
		if d.Leverage > maxLeverage {
			logger.Infof("⚠️  [Leverage Fallback] %s leverage exceeded (%dx > %dx), auto-adjusting to limit %dx",
				d.Symbol, d.Leverage, maxLeverage, maxLeverage)
			d.Leverage = maxLeverage
		}
		if d.PositionSizeUSD <= 0 {
			return fmt.Errorf("position size must be greater than 0: %.2f", d.PositionSizeUSD)
		}

		const minPositionSizeGeneral = 12.0
		const minPositionSizeBTCETH = 60.0

		if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
			if d.PositionSizeUSD < minPositionSizeBTCETH {
				return fmt.Errorf("%s opening amount too small (%.2f USDT), must be ≥%.2f USDT", d.Symbol, d.PositionSizeUSD, minPositionSizeBTCETH)
			}
		} else {
			if d.PositionSizeUSD < minPositionSizeGeneral {
				return fmt.Errorf("opening amount too small (%.2f USDT), must be ≥%.2f USDT", d.PositionSizeUSD, minPositionSizeGeneral)
			}
		}

		tolerance := maxPositionValue * 0.01
		if d.PositionSizeUSD > maxPositionValue+tolerance {
			switch {
			case d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT":
				return fmt.Errorf("BTC/ETH single coin position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", maxPositionValue, posRatio, d.PositionSizeUSD)
			case market.IsXyzDexAsset(d.Symbol):
				return fmt.Errorf("%s position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", d.Symbol, maxPositionValue, posRatio, d.PositionSizeUSD)
			default:
				return fmt.Errorf("altcoin single coin position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", maxPositionValue, posRatio, d.PositionSizeUSD)
			}
		}
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			return fmt.Errorf("stop loss and take profit must be greater than 0")
		}

		if d.Action == "open_long" {
			if d.StopLoss >= d.TakeProfit {
				return fmt.Errorf("for long positions, stop loss price must be less than take profit price")
			}
		} else {
			if d.StopLoss <= d.TakeProfit {
				return fmt.Errorf("for short positions, stop loss price must be greater than take profit price")
			}
		}

		var entryPrice float64
		if d.Action == "open_long" {
			entryPrice = d.StopLoss + (d.TakeProfit-d.StopLoss)*0.2
		} else {
			entryPrice = d.StopLoss - (d.StopLoss-d.TakeProfit)*0.2
		}

		var riskPercent, rewardPercent, riskRewardRatio float64
		if d.Action == "open_long" {
			riskPercent = (entryPrice - d.StopLoss) / entryPrice * 100
			rewardPercent = (d.TakeProfit - entryPrice) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		} else {
			riskPercent = (d.StopLoss - entryPrice) / entryPrice * 100
			rewardPercent = (entryPrice - d.TakeProfit) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		}

		if riskRewardRatio < 3.0 {
			return fmt.Errorf("risk/reward ratio too low (%.2f:1), must be ≥3.0:1 [risk: %.2f%% reward: %.2f%%] [stop loss: %.2f take profit: %.2f]",
				riskRewardRatio, riskPercent, rewardPercent, d.StopLoss, d.TakeProfit)
		}

		// Risk cap: bound size so a stop-out loses at most RiskPerTradePct%% of equity.
		riskSize := RiskCappedPositionSize(accountEquity, entryPrice, d.StopLoss, d.Action == "open_long", riskPerTradePct, maxPositionValue)
		minSize := minPositionSizeGeneral
		if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
			minSize = minPositionSizeBTCETH
		}
		if riskSize < minSize {
			return fmt.Errorf("stop too wide: at min size %.0f USDT the position would risk >%.1f%% of equity; tighten the stop or output wait", minSize, riskPerTradePct)
		}
		if d.PositionSizeUSD > riskSize {
			d.PositionSizeUSD = riskSize
		}
	}

	return nil
}
