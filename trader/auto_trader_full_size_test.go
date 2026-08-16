package trader

import (
	"nofx/kernel"
	"nofx/store"
	"testing"
)

func TestApplyAutopilotFullSizeOpenForClaw402(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "vergex_signal"
	cfg.RiskControl.BTCETHMaxLeverage = 10
	cfg.RiskControl.AltcoinMaxLeverage = 10
	cfg.RiskControl.BTCETHMaxPositionValueRatio = 10
	cfg.RiskControl.AltcoinMaxPositionValueRatio = 10

	at := &AutoTrader{config: AutoTraderConfig{StrategyConfig: &cfg}}
	decision := &kernel.Decision{
		Symbol:          "xyz:INTC",
		Action:          "open_long",
		Leverage:        3,
		PositionSizeUSD: 12,
	}

	at.applyAutopilotFullSizeOpen(decision, 29.8)

	if decision.Leverage != 10 {
		t.Fatalf("expected leverage to be forced to 10x, got %dx", decision.Leverage)
	}
	if decision.PositionSizeUSD != 298 {
		t.Fatalf("expected position size to use full 10x notional 298, got %.2f", decision.PositionSizeUSD)
	}
}

// TestApplyAutopilotRiskCapsWideStop verifies the autopilot sizes a position by
// the stop-distance risk cap (#1): a 10%% stop caps notional below the full
// equity×ratio value, so wide-stop trades risk no more than RiskPerTradePct%% of
// equity.
func TestApplyAutopilotRiskCapsWideStop(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "vergex_signal"
	cfg.RiskControl.AltcoinMaxLeverage = 10
	cfg.RiskControl.AltcoinMaxPositionValueRatio = 1.0
	cfg.RiskControl.RiskPerTradePct = 3.0
	at := &AutoTrader{config: AutoTraderConfig{StrategyConfig: &cfg}}

	// xyz:ACE (major tier) but ratio forced to 1.0 for a clean check. entry is
	// derived as stop + 0.2*(tp-stop) = 100, stop at 90 -> 10%% stop.
	decision := &kernel.Decision{
		Symbol: "xyz:ACE", Action: "open_long", Leverage: 10,
		PositionSizeUSD: 29.8, StopLoss: 90, TakeProfit: 140,
	}
	at.applyAutopilotFullSizeOpen(decision, 29.8)
	// full notional = 29.8 x 1.0 = 29.8; risk cap = 3%%*29.8 / 0.10 = 8.94.
	if got := decision.PositionSizeUSD; !approxFloat(got, 8.94, 0.01) {
		t.Fatalf("expected risk-capped size 8.94, got %.2f", got)
	}
}

// TestRiskScaleForDrawdown verifies the account-level drawdown size-trim (#2):
// full size near peak, halved past 15%%, quartered past 30%% below peak.
func TestRiskScaleForDrawdown(t *testing.T) {
	at := &AutoTrader{}
	at.peakPnLCacheMutex.Lock()
	at.peakEquity = 100
	at.peakPnLCacheMutex.Unlock()

	if got := at.riskScaleForDrawdown(100); got != 1.0 {
		t.Errorf("at peak scalar = %.2f, want 1.0", got)
	}
	if got := at.riskScaleForDrawdown(80); got != 0.5 {
		t.Errorf("20%% drawdown scalar = %.2f, want 0.5", got)
	}
	if got := at.riskScaleForDrawdown(60); got != 0.25 {
		t.Errorf("40%% drawdown scalar = %.2f, want 0.25", got)
	}

	// No tracked peak -> full size.
	fresh := &AutoTrader{}
	if got := fresh.riskScaleForDrawdown(50); got != 1.0 {
		t.Errorf("no-peak scalar = %.2f, want 1.0", got)
	}
}

// TestEnforcePositionValueRatioAppliesDrawdownScalar verifies the position-value
// cap is reduced by the drawdown trim during a drawdown (#2).
func TestEnforcePositionValueRatioAppliesDrawdownScalar(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.RiskControl.AltcoinMaxPositionValueRatio = 1.0
	at := &AutoTrader{config: AutoTraderConfig{StrategyConfig: &cfg}}
	at.peakPnLCacheMutex.Lock()
	at.peakEquity = 100
	at.peakPnLCacheMutex.Unlock()

	// Current equity 80, peak 100 -> 20%% drawdown -> scalar 0.5.
	// cap = 80 x 1.0 x 0.5 = 40.
	capped, wasCapped := at.enforcePositionValueRatio(100, 80, "SOLUSDT")
	if !wasCapped || !approxFloat(capped, 40, 0.01) {
		t.Fatalf("capped=%.2f wasCapped=%v, want 40/true", capped, wasCapped)
	}
}

// approxFloat is a tiny equality helper for float comparisons.
func approxFloat(got, want, eps float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= eps
}

func TestApplyAutopilotFullSizeOpenSkipsNonClaw402Strategies(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "static"
	cfg.RiskControl.BTCETHMaxLeverage = 10
	cfg.RiskControl.AltcoinMaxLeverage = 10

	at := &AutoTrader{config: AutoTraderConfig{StrategyConfig: &cfg}}
	decision := &kernel.Decision{
		Symbol:          "BTCUSDT",
		Action:          "open_long",
		Leverage:        3,
		PositionSizeUSD: 12,
	}

	at.applyAutopilotFullSizeOpen(decision, 29.8)

	if decision.Leverage != 3 || decision.PositionSizeUSD != 12 {
		t.Fatalf("non-Claw402 strategies should not be rewritten, got leverage=%d size=%.2f", decision.Leverage, decision.PositionSizeUSD)
	}
}
