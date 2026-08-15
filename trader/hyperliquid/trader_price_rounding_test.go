package hyperliquid

import (
	"math"
	"sync"
	"testing"

	"github.com/sonirico/go-hyperliquid"
)

// testPriceTrader builds a HyperliquidTrader with a meta universe containing the
// low-priced coins (HEMI, ACE) plus a normal mid-priced coin (BTC) so that
// roundPriceForOrder can resolve each coin's szDecimals.
func testPriceTrader() *HyperliquidTrader {
	return &HyperliquidTrader{
		meta: &hyperliquid.Meta{
			Universe: []hyperliquid.AssetInfo{
				{Name: "HEMI", SzDecimals: 0},
				{Name: "ACE", SzDecimals: 0},
				{Name: "BTC", SzDecimals: 5},
				{Name: "ETH", SzDecimals: 4},
			},
		},
		metaMutex: sync.RWMutex{},
	}
}

// isMultipleOfTick reports whether price is a whole multiple of tick.
func isMultipleOfTick(price, tick float64) bool {
	const eps = 1e-6
	n := price / tick
	return math.Abs(n-math.Round(n)) < eps
}

// TestRoundPriceForOrder_LowPricedCoins verifies the price-rounding fix for
// HEMI (~0.0065) and ACE (~0.174): the rounded aggressive price must land on a
// valid Hyperliquid price tick (a multiple of 10^-(6-szDecimals)) and stay within
// ~1-2% of the market price (the aggressive offset is 1%).
func TestRoundPriceForOrder_LowPricedCoins(t *testing.T) {
	ht := testPriceTrader()

	cases := []struct {
		name  string
		coin  string
		price float64 // current market price
	}{
		{"HEMI long", "HEMI", 0.006479},
		{"HEMI short", "HEMI", 0.006479},
		{"ACE long", "ACE", 0.174},
		{"ACE short", "ACE", 0.174},
		{"BTC long", "BTC", 65000},
		{"ETH short", "ETH", 3500},
	}

	for _, tc := range cases {
		szDecimals := ht.getSzDecimals(tc.coin)
		maxDecimals := 6 - szDecimals
		if maxDecimals < 0 {
			maxDecimals = 0
		}
		tick := math.Pow(10, -float64(maxDecimals))

		// Simulate both the buy (1.01x) and sell (0.99x) aggressive factors.
		factors := []float64{1.01, 0.99}
		for _, f := range factors {
			aggressive := tc.price * f
			got := ht.roundPriceForOrder(tc.coin, aggressive)

			if got <= 0 {
				t.Fatalf("%s: roundPriceForOrder returned non-positive price %.8f", tc.name, got)
			}
			if !isMultipleOfTick(got, tick) {
				t.Errorf("%s: rounded price %.8f is NOT a multiple of price tick %.10f (szDecimals=%d)",
					tc.name, got, tick, szDecimals)
			}

			// Aggressive price should be within ~2% of market (offset is 1%).
			dev := math.Abs(got-tc.price) / tc.price
			if dev > 0.02 {
				t.Errorf("%s: rounded price %.8f deviates %.2f%% from market %.8f (want <=2%%)",
					tc.name, got, dev*100, tc.price)
			}
		}
	}
}

// TestRoundPriceForOrder_RejectsZeroPrice verifies a zero price stays zero.
func TestRoundPriceForOrder_RejectsZeroPrice(t *testing.T) {
	ht := testPriceTrader()
	if got := ht.roundPriceForOrder("HEMI", 0); got != 0 {
		t.Fatalf("expected 0 for zero price, got %v", got)
	}
}

// TestRoundPriceForOrder_XYZKeepsSigfigs verifies the xyz dex path keeps the
// existing 5-significant-figure rounding and is unchanged by the tick fix.
func TestRoundPriceForOrder_XYZKeepsSigfigs(t *testing.T) {
	ht := testPriceTrader()
	got := ht.roundPriceForOrder("xyz:AAPL", 212.345678)
	if got != ht.roundPriceToSigfigs(212.345678) {
		t.Fatalf("xyz dex rounding changed: got %v, want %v", got, ht.roundPriceToSigfigs(212.345678))
	}
}
