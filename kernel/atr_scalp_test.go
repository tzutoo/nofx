package kernel

import (
	"testing"

	"nofx/market"
	"nofx/store"
)

// TestATRStopTarget verifies the side-aware ATR stop/target arithmetic and its
// degenerate cases (long stop / short target must stay on the correct side of 0).
func TestATRStopTarget(t *testing.T) {
	// Long: entry 100, atr 2, stop 1.5x, target 2x -> stop 97, target 104.
	if stop, target, ok := ATRStopTarget(100, 2, 1.5, 2.0, true); !ok || stop != 97 || target != 104 {
		t.Fatalf("long = %.2f/%.2f ok=%v, want 97/104 true", stop, target, ok)
	}
	// Short: stop above entry, target below -> stop 103, target 96.
	if stop, target, ok := ATRStopTarget(100, 2, 1.5, 2.0, false); !ok || stop != 103 || target != 96 {
		t.Fatalf("short = %.2f/%.2f ok=%v, want 103/96 true", stop, target, ok)
	}
	// Degenerate long: stop below entry would be <= 0.
	if _, _, ok := ATRStopTarget(1, 2, 1.5, 2.0, true); ok {
		t.Fatal("expected long degenerate (stop <= 0) to be invalid")
	}
	// Degenerate short: target below entry would be <= 0.
	if _, _, ok := ATRStopTarget(1, 2, 1.5, 2.0, false); ok {
		t.Fatal("expected short degenerate (target <= 0) to be invalid")
	}
	// atr14 <= 0 is invalid.
	if _, _, ok := ATRStopTarget(100, 0, 1.5, 2.0, true); ok {
		t.Fatal("expected atr14<=0 to be invalid")
	}
	// Zero multiplier is invalid.
	if _, _, ok := ATRStopTarget(100, 2, 0, 2.0, true); ok {
		t.Fatal("expected zero stop multiplier to be invalid")
	}
}

// TestValidateDecisionConfigRRLower proves the R/R floor is config-driven (was
// hardcoded 3.0). The synthetic 20%-of-span inferred entry makes R/R a constant
// 4.0 for any valid SL/TP pair, so a floor of 1.2 or 3.0 both pass while a floor
// of 5.0 rejects — demonstrating the floor value is honored, not hardcoded.
func TestValidateDecisionConfigRRLower(t *testing.T) {
	mk := func() Decision {
		return Decision{
			Symbol: "SOLUSDT", Action: "open_long", Leverage: 5,
			PositionSizeUSD: 100, StopLoss: 90, TakeProfit: 110,
		}
	}
	d1 := mk()
	if err := validateDecision(&d1, 100, 10, 5, 1.0, 1.0, 3.0, 1.2); err != nil {
		t.Fatalf("floor 1.2 should pass: %v", err)
	}
	d2 := mk()
	if err := validateDecision(&d2, 100, 10, 5, 1.0, 1.0, 3.0, 3.0); err != nil {
		t.Fatalf("floor 3.0 should pass (R/R is 4.0): %v", err)
	}
	d3 := mk()
	if err := validateDecision(&d3, 100, 10, 5, 1.0, 1.0, 3.0, 5.0); err == nil {
		t.Fatal("floor 5.0 should reject (R/R 4.0 < 5.0)")
	}
}

// TestFilterCandidatesByATRCap verifies the ATR eligibility hard-exclude: a coin
// whose 15m ATR%/price exceeds the cap is dropped from the candidate pool, while
// a low-ATR coin is kept. Held positions are not part of CandidateCoins here.
func TestFilterCandidatesByATRCap(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en") // cap 3.0, PrimaryTimeframe "15m"
	engine := NewStrategyEngine(&cfg)
	ctx := &Context{
		CandidateCoins: []CandidateCoin{
			{Symbol: "HEMI"},  // 5% ATR -> excluded
			{Symbol: "KAITO"}, // 1% ATR -> kept
		},
		MarketDataMap: map[string]*market.Data{
			"HEMI": {
				CurrentPrice: 1.0,
				TimeframeData: map[string]*market.TimeframeSeriesData{
					"15m": {ATR14: 0.05},
				},
			},
			"KAITO": {
				CurrentPrice: 1.0,
				TimeframeData: map[string]*market.TimeframeSeriesData{
					"15m": {ATR14: 0.01},
				},
			},
		},
	}
	filterCandidatesByATRCap(ctx, engine)
	if len(ctx.CandidateCoins) != 1 || ctx.CandidateCoins[0].Symbol != "KAITO" {
		t.Fatalf("expected only KAITO to remain, got %v", ctx.CandidateCoins)
	}
}

// TestFilterCandidatesByATRCapDisabled verifies a zero cap disables the filter
// (legacy opt-in) and leaves all candidates untouched.
func TestFilterCandidatesByATRCapDisabled(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.RiskControl.ATREligibilityCapPct = 0 // disabled
	engine := NewStrategyEngine(&cfg)
	ctx := &Context{
		CandidateCoins: []CandidateCoin{{Symbol: "HEMI"}, {Symbol: "KAITO"}},
		MarketDataMap: map[string]*market.Data{
			"HEMI": {CurrentPrice: 1.0, TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {ATR14: 0.05}}},
		},
	}
	filterCandidatesByATRCap(ctx, engine)
	if len(ctx.CandidateCoins) != 2 {
		t.Fatalf("expected no filtering when cap disabled, got %v", ctx.CandidateCoins)
	}
}
