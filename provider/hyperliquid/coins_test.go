package hyperliquid

import "testing"

func TestXYZCategoryIndexLikeSymbolsClassifiedAsIndex(t *testing.T) {
	indexLikes := []string{
		// From the coins.go index case.
		"SPX", "NDX", "DJI", "VIX", "DAX", "FTSE", "NIKKEI", "HSI", "CSI300",
		"XYZ100", "XYZ25", "XYZ50",
		// Reconciled from handler_klines.go hyperliquidXYZCategory index list.
		"SP500", "JP225", "KR200", "DXY", "XLE", "EWY", "EWJ", "EWZ", "EWT",
		"NIFTY", "IBOV",
	}
	for _, base := range indexLikes {
		if got := XYZCategory(base); got != "index" {
			t.Errorf("XYZCategory(%q) = %q, want \"index\"", base, got)
		}
	}
}

func TestXYZCategoryMappableStock(t *testing.T) {
	if got := XYZCategory("NVDA"); got != "stock" {
		t.Errorf("XYZCategory(NVDA) = %q, want \"stock\"", got)
	}
	if got := XYZCategory("XYZ:AAPL"); got != "stock" {
		t.Errorf("XYZCategory(XYZ:AAPL) = %q, want \"stock\"", got)
	}
}
