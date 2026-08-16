package signal

import (
	"testing"

	"nofx/provider/hyperliquid"
)

// TestTradableSnapshot verifies the ingest filter that excludes delisted /
// no-live-market assets (zero or negative mark price) so they never enter the
// snapshot and can't surface in the terminal ranking or dashboard panels.
func TestTradableSnapshot(t *testing.T) {
	cases := []struct {
		name string
		m    hyperliquid.MarketSnapshot
		want bool
	}{
		{name: "delisted zero mark", m: hyperliquid.MarketSnapshot{Symbol: "MEW", MarkPx: 0, IsDelisted: true}, want: false},
		{name: "delisted stale positive mark", m: hyperliquid.MarketSnapshot{Symbol: "MEW", MarkPx: 0.00064, IsDelisted: true}, want: false},
		{name: "negative mark", m: hyperliquid.MarketSnapshot{Symbol: "X", MarkPx: -1}, want: false},
		{name: "live crypto", m: hyperliquid.MarketSnapshot{Symbol: "BTC", MarkPx: 63000}, want: true},
		{name: "live low-price", m: hyperliquid.MarketSnapshot{Symbol: "DOGE", MarkPx: 0.12}, want: true},
	}
	for _, tc := range cases {
		if got := tradableSnapshot(tc.m); got != tc.want {
			t.Errorf("%s: tradableSnapshot(MarkPx=%.6f) = %v, want %v", tc.name, tc.m.MarkPx, got, tc.want)
		}
	}
}
