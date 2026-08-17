package trader

import "testing"

func TestDrawdownCloseArmsOnPriceBasisOnly(t *testing.T) {
	cases := []struct {
		name        string
		pricePnLPct float64
		armPct      float64
		drawdownPct float64
		shouldClose bool
	}{
		// +0.5% price move (what +5% margin at 10x used to arm on) must NOT arm.
		{"tiny price gain big drawdown", 0.5, 5.0, 60.0, false},
		// Armed only past armPct, and still needs the 40% giveback.
		{"real gain small drawdown", 6.0, 5.0, 20.0, false},
		{"real gain big drawdown", 6.0, 5.0, 45.0, true},
		{"at threshold not armed", 5.0, 5.0, 45.0, false},
		{"loss never triggers", -3.0, 5.0, 80.0, false},
	}
	for _, c := range cases {
		if got := shouldDrawdownClose(c.pricePnLPct, c.armPct, c.drawdownPct); got != c.shouldClose {
			t.Fatalf("%s: shouldDrawdownClose(%.1f, %.1f, %.1f) = %v, want %v",
				c.name, c.pricePnLPct, c.armPct, c.drawdownPct, got, c.shouldClose)
		}
	}
}

// TestDrawdownArmPct verifies the ATR-relative arming threshold: 1×ATR14 as a %
// of entry, with a fixed 1.5% fallback when ATR/entry is missing or sub-1%.
func TestDrawdownArmPct(t *testing.T) {
	at := &AutoTrader{}
	if got := at.drawdownArmPct(100, 2.5); got != 2.5 {
		t.Fatalf("drawdownArmPct(100, 2.5) = %.2f, want 2.5", got)
	}
	if got := at.drawdownArmPct(100, 0.05); got != 1.5 {
		t.Fatalf("drawdownArmPct(100, 0.05) = %.2f, want 1.5 (fallback)", got)
	}
	if got := at.drawdownArmPct(0, 0); got != 1.5 {
		t.Fatalf("drawdownArmPct(0, 0) = %.2f, want 1.5 (fallback)", got)
	}
}
