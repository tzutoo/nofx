package kernel

import (
	"testing"
)

// TestLeverageFallback tests automatic correction when leverage exceeds limit
func TestLeverageFallback(t *testing.T) {
	tests := []struct {
		name            string
		decision        Decision
		accountEquity   float64
		btcEthLeverage  int
		altcoinLeverage int
		wantLeverage    int // Expected leverage after correction
		wantError       bool
	}{
		{
			name: "Altcoin leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 100,
				StopLoss:        90,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5, // Limit 5x
			wantLeverage:    5, // Should be corrected to 5
			wantError:       false,
		},
		{
			name: "BTC leverage exceeded - auto-correct to limit",
			decision: Decision{
				Symbol:          "BTCUSDT",
				Action:          "open_long",
				Leverage:        20, // Exceeds limit
				PositionSizeUSD: 1000,
				StopLoss:        90000,
				TakeProfit:      110000,
			},
			accountEquity:   100,
			btcEthLeverage:  10, // Limit 10x
			altcoinLeverage: 5,
			wantLeverage:    10, // Should be corrected to 10
			wantError:       false,
		},
		{
			name: "Leverage within limit - no correction",
			decision: Decision{
				Symbol:          "ETHUSDT",
				Action:          "open_short",
				Leverage:        5, // Not exceeded
				PositionSizeUSD: 500,
				StopLoss:        3900,
				TakeProfit:      3000,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    5, // Stays unchanged
			wantError:       false,
		},
		{
			name: "Leverage is 0 - should error",
			decision: Decision{
				Symbol:          "SOLUSDT",
				Action:          "open_long",
				Leverage:        0, // Invalid
				PositionSizeUSD: 100,
				StopLoss:        50,
				TakeProfit:      200,
			},
			accountEquity:   100,
			btcEthLeverage:  10,
			altcoinLeverage: 5,
			wantLeverage:    0,
			wantError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use default position value ratios for testing (10x for BTC/ETH, 1.5x for altcoins)
			err := validateDecision(&tt.decision, tt.accountEquity, tt.btcEthLeverage, tt.altcoinLeverage, 10.0, 1.5, 3.0)

			// Check error status
			if (err != nil) != tt.wantError {
				t.Errorf("validateDecision() error = %v, wantError %v", err, tt.wantError)
				return
			}

			// If shouldn't error, check if leverage was correctly corrected
			if !tt.wantError && tt.decision.Leverage != tt.wantLeverage {
				t.Errorf("Leverage not corrected: got %d, want %d", tt.decision.Leverage, tt.wantLeverage)
			}
		})
	}
}

// TestRiskCappedPositionSize verifies the stop-distance risk cap: a wider stop
// yields a smaller allowed notional, and the cap never exceeds the notional
// ceiling (equity × ratio). Long and short both covered.
func TestRiskCappedPositionSize(t *testing.T) {
	// equity 100, budget 3%% -> max 3 USDT risk. Tight stop (2%%) allows big size,
	// wide stop (10%%) caps it down, both <= notional ceiling (equity x 1 = 100).
	tight := RiskCappedPositionSize(100, 100, 98, true, 3.0, 100)   // risk 2%% -> 3/0.02 = 150 -> cap 100
	wide := RiskCappedPositionSize(100, 100, 90, true, 3.0, 100)    // risk 10%% -> 3/0.10 = 30
	short := RiskCappedPositionSize(100, 100, 110, false, 3.0, 100) // short, stop above -> 3/0.10 = 30
	if !approxFloat(tight, 100, 1e-9) {
		t.Errorf("tight stop cap = %.2f, want 100 (clamped to notional ceiling)", tight)
	}
	if !approxFloat(wide, 30, 1e-9) {
		t.Errorf("wide long stop cap = %.2f, want 30", wide)
	}
	if !approxFloat(short, 30, 1e-9) {
		t.Errorf("wide short stop cap = %.2f, want 30", short)
	}
}

// TestValidateDecisionRiskCapsSize verifies validateDecision clamps an oversized
// position down to the stop-risk budget and rejects a stop too wide to satisfy
// the budget at the minimum position size.
func TestValidateDecisionRiskCapsSize(t *testing.T) {
	// 100 equity, 3%% budget -> 3 USDT max loss. A 10%% stop (entry 100, stop 90)
	// caps notional at 3/0.10 = 30 USDT, below the proposed 80.
	decision := Decision{
		Symbol: "SOLUSDT", Action: "open_long", Leverage: 5,
		PositionSizeUSD: 80, StopLoss: 90, TakeProfit: 140,
	}
	if err := validateDecision(&decision, 100, 10, 5, 1.0, 1.0, 3.0); err != nil {
		t.Fatalf("validateDecision should clamp, not error: %v", err)
	}
	if !approxFloat(decision.PositionSizeUSD, 30, 0.01) {
		t.Errorf("clamped size = %.2f, want 30 (3%% of 100 / 10%% stop)", decision.PositionSizeUSD)
	}

	// A stop so wide that even min size (12) risks >3%% -> rejected.
	wide := Decision{
		Symbol: "SOLUSDT", Action: "open_long", Leverage: 5,
		PositionSizeUSD: 100, StopLoss: 50, TakeProfit: 200, // ~37%% stop
	}
	if err := validateDecision(&wide, 100, 10, 5, 1.0, 1.0, 3.0); err == nil {
		t.Fatal("expected stop-too-wide rejection")
	}
}

// TestValidateDecisionTinyAccountEdgeCase verifies the small-account + wide-stop
// edge case (#3): on a tiny account the risk budget cannot be met at min size,
// so a wide stop is rejected (downgrade to `wait`), while a tight stop still
// allows a valid position capped within equity×ratio.
func TestValidateDecisionTinyAccountEdgeCase(t *testing.T) {
	cases := []struct {
		name     string
		decision Decision
		wantErr  bool
	}{
		{
			name: "tiny account + wide stop -> reject (downgrade to wait)",
			decision: Decision{Symbol: "SOLUSDT", Action: "open_long", Leverage: 5,
				PositionSizeUSD: 30, StopLoss: 90, TakeProfit: 140}, // ~10% stop
			wantErr: true,
		},
		{
			name: "tiny account + tight stop -> allowed within cap",
			decision: Decision{Symbol: "SOLUSDT", Action: "open_long", Leverage: 5,
				PositionSizeUSD: 30, StopLoss: 96, TakeProfit: 100}, // ~0.8% stop
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// equity 30, altcoin ratio 1.0 -> notional cap 30, budget 3%%.
			err := validateDecision(&tc.decision, 30, 10, 5, 1.0, 1.0, 3.0)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateDecision() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && tc.decision.PositionSizeUSD > 30.3 {
				t.Errorf("size %.2f exceeded notional cap 30.3", tc.decision.PositionSizeUSD)
			}
		})
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

func TestClaw402XyzAllowsFullTenXNotional(t *testing.T) {
	decision := Decision{
		Symbol:          "xyz:SP500",
		Action:          "open_long",
		Leverage:        10,
		PositionSizeUSD: 306.8,
		StopLoss:        95,
		TakeProfit:      120,
	}

	if err := validateDecision(&decision, 30.68, 10, 10, 10.0, 10.0, 3.0); err != nil {
		t.Fatalf("xyz TradeFi Claw402 full 10x notional should pass validation: %v", err)
	}
}

// contains checks if string contains substring (helper function)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
