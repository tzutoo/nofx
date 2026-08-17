package hyperliquid

import (
	"sync"
	"testing"
)

// testXyzTrader builds a HyperliquidTrader with a cached xyz dex meta universe
// so getXyzAssetIndex can resolve known xyz assets without hitting the network.
func testXyzTrader() *HyperliquidTrader {
	return &HyperliquidTrader{
		xyzMeta: &xyzDexMeta{
			Universe: []xyzAssetInfo{
				{Name: "xyz:TSLA", SzDecimals: 1},
				{Name: "xyz:MSTR", SzDecimals: 0},
				{Name: "xyz:PURRDAT", SzDecimals: 0},
				{Name: "xyz:ACE", SzDecimals: 0},
				{Name: "xyz:GOLD", SzDecimals: 1},
			},
		},
		xyzMetaMutex: sync.RWMutex{},
	}
}

// TestXYZPerpDexIndex verifies the mainnet default perp-dex index (1) and that
// a resolved index is honored (testnet puts xyz at a much larger index).
func TestXYZPerpDexIndex(t *testing.T) {
	if defaultXyzPerpDexIndex != 1 {
		t.Fatalf("defaultXyzPerpDexIndex = %d, want 1", defaultXyzPerpDexIndex)
	}
	ht := testXyzTrader() // xyzPerpDexIndex == 0 -> unresolved -> mainnet default
	if got := ht.xyzDexAssetIndex(0); got != 110000 {
		t.Fatalf("unresolved xyzDexAssetIndex(0) = %d, want 110000", got)
	}
	ht.xyzPerpDexIndex = 65 // testnet
	if got := ht.xyzDexAssetIndex(0); got != 100000+65*10000 {
		t.Fatalf("resolved xyzDexAssetIndex(0) = %d, want %d", got, 100000+65*10000)
	}
}

// TestXYZDexAssetIndex verifies the HIP-3 perp-dex asset index formula:
// 100000 + perpDexIndex*10000 + metaIndex.
func TestXYZDexAssetIndex(t *testing.T) {
	ht := testXyzTrader()
	cases := map[int]int{
		0:   110000,
		1:   110001,
		5:   110005,
		999: 110999,
	}
	for metaIndex, want := range cases {
		if got := ht.xyzDexAssetIndex(metaIndex); got != want {
			t.Fatalf("xyzDexAssetIndex(%d) = %d, want %d", metaIndex, got, want)
		}
	}
}

// TestSetLeverageXYZAssetIndex verifies that known xyz assets resolve to the
// correct HIP-3 asset index (the value used to build UpdateLeverageAction).
// This is the value that the old path could not compute, because the SDK's
// CoinToAsset lookup only knows core perp coins and returns "coin not found"
// for xyz assets, leaving the account at its prior leverage.
func TestSetLeverageXYZAssetIndex(t *testing.T) {
	ht := testXyzTrader()

	cases := map[string]int{
		"xyz:TSLA":    110000,
		"xyz:MSTR":    110001,
		"xyz:PURRDAT": 110002,
		"xyz:ACE":     110003,
		"xyz:GOLD":    110004,
	}
	for coin, want := range cases {
		metaIndex := ht.getXyzAssetIndex(coin)
		if metaIndex < 0 {
			t.Fatalf("getXyzAssetIndex(%q) = %d, want >= 0", coin, metaIndex)
		}
		if got := ht.xyzDexAssetIndex(metaIndex); got != want {
			t.Fatalf("xyz asset %s: asset index = %d (metaIndex=%d), want %d",
				coin, got, metaIndex, want)
		}
	}
}

// TestSetLeverageXYZUnknownAsset verifies an unknown xyz asset is rejected by
// the meta lookup (so SetLeverage would fail the order rather than silently
// continuing at an unverified leverage).
func TestSetLeverageXYZUnknownAsset(t *testing.T) {
	ht := testXyzTrader()
	if got := ht.getXyzAssetIndex("xyz:NOTALISTEDASSET"); got >= 0 {
		t.Fatalf("getXyzAssetIndex(unknown) = %d, want < 0", got)
	}
}
