package vergex

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestParseSignalRankingAndFilterTradFiItems(t *testing.T) {
	body := []byte(`{
		"data": {
			"rankings": [
				{"marketType":"hip3-perp","symbol":"AAPL","bias":"long","confidence":0.88,"compositeZ":1.75},
				{"marketType":"stock","symbol":"NVDA","bias":"long","confidence":0.81,"compositeZ":1.25},
				{"market_type":"core_perp","symbol":"BTC","bias":"short","score":0.91}
			]
		}
	}`)

	ranking, err := ParseSignalRanking(body)
	if err != nil {
		t.Fatalf("ParseSignalRanking returned error: %v", err)
	}
	if len(ranking.Items) != 3 {
		t.Fatalf("items len = %d, want 3", len(ranking.Items))
	}
	if ranking.Items[0].Symbol != "AAPL" || ranking.Items[0].MarketType != "hip3-perp" || ranking.Items[0].Bias != "long" {
		t.Fatalf("unexpected first item: %+v", ranking.Items[0])
	}

	items := FilterTradFiItems(ranking.Items, "hip3_perp", 5)
	if len(items) != 2 {
		t.Fatalf("filtered len = %d, want 2", len(items))
	}
	if got := TradableSymbol(items[0].Symbol); got != "xyz:AAPL" {
		t.Fatalf("TradableSymbol = %q, want xyz:AAPL", got)
	}
	if got := TradableSymbol(items[1].Symbol); got != "xyz:NVDA" {
		t.Fatalf("TradableSymbol = %q, want xyz:NVDA", got)
	}
}

func TestFilterTradFiItemsAllowsFullClaw402Board(t *testing.T) {
	items := make([]SignalRankItem, 0, 35)
	for i := 1; i <= 35; i++ {
		items = append(items, SignalRankItem{
			Rank:       i,
			Symbol:     fmt.Sprintf("xyz:STK%02d", i),
			MarketType: "hip3_perp",
			Bias:       "bullish",
		})
	}

	filtered := FilterTradFiItems(items, "hip3_perp", 30)
	if len(filtered) != 30 {
		t.Fatalf("filtered len = %d, want 30", len(filtered))
	}
	if filtered[0].Symbol != "STK01" || filtered[29].Symbol != "STK30" {
		t.Fatalf("unexpected filtered bounds: first=%q last=%q", filtered[0].Symbol, filtered[29].Symbol)
	}

	capped := FilterTradFiItems(items, "hip3_perp", 35)
	if len(capped) != MaxSignalRankingItems {
		t.Fatalf("capped len = %d, want %d", len(capped), MaxSignalRankingItems)
	}
}

func TestParseSignalRankingReadsNestedMarketShape(t *testing.T) {
	body := []byte(`{
		"data": {
			"items": [
				{"market":{"marketType":"hip3_perp","symbol":"xyz:NBIS"},"symbol":"xyz:NBIS","bias":"bullish","compositeZ":1.05,"rank":5},
				{"market":{"marketType":"hip3_perp","symbol":"xyz:DRAM"},"symbol":"xyz:DRAM","bias":"bullish","compositeZ":0.47,"rank":10},
				{"market":{"marketType":"core_perp","symbol":"BTC"},"symbol":"BTC","bias":"bearish","compositeZ":-0.08,"rank":12}
			]
		}
	}`)

	ranking, err := ParseSignalRanking(body)
	if err != nil {
		t.Fatalf("ParseSignalRanking returned error: %v", err)
	}
	allItems := FilterSignalRankingItems(ranking.Items, "all", 30)
	if len(allItems) != 3 {
		t.Fatalf("all filtered len = %d, want 3: %+v", len(allItems), allItems)
	}
	if allItems[2].Symbol != "BTC" || allItems[2].MarketType != "core_perp" || allItems[2].Category != "crypto" {
		t.Fatalf("unexpected crypto item: %+v", allItems[2])
	}
	items := FilterTradFiItems(ranking.Items, "hip3_perp", 30)
	if len(items) != 2 {
		t.Fatalf("filtered len = %d, want 2: %+v", len(items), items)
	}
	if items[0].Symbol != "NBIS" || items[0].MarketType != "hip3_perp" {
		t.Fatalf("unexpected first item: %+v", items[0])
	}
	if items[1].Symbol != "DRAM" || items[1].MarketType != "hip3_perp" {
		t.Fatalf("unexpected second item: %+v", items[1])
	}
}

func TestMarketSymbolPreservesHIP3XYZPrefix(t *testing.T) {
	if got := MarketSymbol("hip3_perp", "INTC"); got != "xyz:INTC" {
		t.Fatalf("MarketSymbol hip3_perp/INTC = %q, want xyz:INTC", got)
	}
	if got := MarketSymbol("hip3_perp", "xyz:skhx"); got != "xyz:SKHX" {
		t.Fatalf("MarketSymbol hip3_perp/xyz:skhx = %q, want xyz:SKHX", got)
	}
	if got := MarketSymbol("core_perp", "BTC"); got != "BTC" {
		t.Fatalf("MarketSymbol core_perp/BTC = %q, want BTC", got)
	}
	if got := TradableSymbolForMarket("core_perp", "BTC"); got != "BTC" {
		t.Fatalf("TradableSymbolForMarket core_perp/BTC = %q, want BTC", got)
	}
	if got := TradableSymbolForMarket("hip3_perp", "INTC"); got != "xyz:INTC" {
		t.Fatalf("TradableSymbolForMarket hip3_perp/INTC = %q, want xyz:INTC", got)
	}
}

func TestMarketSymbolPrefixesByAssetType(t *testing.T) {
	// Crypto core_perp symbols (present in HL core dex) must be BARE.
	for _, sym := range []string{"2Z", "ACE", "PUMP", "XAI", "INJ", "KAITO", "STABLE", "BTC"} {
		if got := MarketSymbol("hip3_perp", sym); got != sym {
			t.Fatalf("MarketSymbol hip3_perp/%s = %q, want bare %q", sym, got, sym)
		}
	}
	// xyz TradeFi assets must keep the xyz: prefix.
	for _, tc := range []struct{ in, want string }{
		{"hip3_perp", "USAR"}, {"hip3_perp", "SP500"}, {"hip3_perp", "NVDA"}, {"hip3_perp", "INTC"},
	} {
		if got := MarketSymbol(tc.in, tc.want); got != "xyz:"+tc.want {
			t.Fatalf("MarketSymbol %s/%s = %q, want xyz:%s", tc.in, tc.want, got, tc.want)
		}
	}
}

func TestAddQueryDefaultsUsesClaw402GatewayParams(t *testing.T) {
	params := url.Values{}
	addQueryDefaults(params, Query{
		MarketType: "hip3_perp",
		Symbol:     "INTC",
		Chain:      "hyperliquid",
		LiqBand:    "15",
	}, true)

	if got := params.Get("marketType"); got != "hip3_perp" {
		t.Fatalf("marketType = %q", got)
	}
	if got := params.Get("symbol"); got != "xyz:INTC" {
		t.Fatalf("symbol = %q, want xyz:INTC", got)
	}
	if got := params.Get("chain"); got != "mainnet" {
		t.Fatalf("chain = %q, want mainnet", got)
	}
	if got := params.Get("liqBand"); got != "15" {
		t.Fatalf("liqBand = %q, want 15", got)
	}
}

func TestQueryChainMapsHyperliquidToVergexMainnet(t *testing.T) {
	if got := QueryChain("hyperliquid"); got != "mainnet" {
		t.Fatalf("QueryChain hyperliquid = %q, want mainnet", got)
	}
}

func TestFormatAnalysisForAIIncludesDetailErrors(t *testing.T) {
	text := FormatAnalysisForAI(&MarketAnalysis{
		Symbol:         "xyz:NVDA",
		QuerySymbol:    "NVDA",
		MarketType:     "stock",
		SignalLabError: "upstream returned status 502",
		HeatmapError:   "market not found",
	})

	if !containsAll(text, "Signal Lab: unavailable", "upstream returned status 502", "Cost/Liquidation Heatmap: unavailable", "market not found") {
		t.Fatalf("formatted analysis did not include detail errors:\n%s", text)
	}
}

func TestFormatAnalysisForAIFormatsVergexDetailsAsMarkdown(t *testing.T) {
	text := FormatAnalysisForAI(&MarketAnalysis{
		Symbol:      "xyz:DRAM",
		QuerySymbol: "DRAM",
		MarketType:  "hip3_perp",
		SignalLab: []byte(`{
			"data": {
				"band": "15",
				"bias": "bullish",
				"compositeZ": 1.41,
				"confidence": "Medium",
				"dimensions": [
					{
						"family": "I Cost & Positioning",
						"label": "Capital-gains overhang",
						"direction": "bullish",
						"strength": "medium",
						"percentile": 80,
						"detail": "price is above aggregate cost"
					}
				]
			}
		}`),
		Heatmap: []byte(`{
			"data": {
				"binStep": 3.2,
				"bins": [
					{"bucketStartPrice": 100, "bucketEndPrice": 103.2, "longCost": 1200000, "shortCost": 1000, "longLiq": 5000, "shortLiq": 700000},
					{"bucketStartPrice": 103.2, "bucketEndPrice": 106.4, "longCost": 1000, "shortCost": 2000, "longLiq": 900000, "shortLiq": 4000}
				]
			}
		}`),
	})

	if !containsAll(text,
		"#### Signal Lab",
		"| Family | Signal | Direction | Strength | Percentile | Detail |",
		"Capital-gains overhang",
		"#### Cost/Liquidation Heatmap",
		"| Price zone | Long cost | Short cost | Long liq | Short liq | Main cluster |",
		"$1.20M",
	) {
		t.Fatalf("formatted analysis is not markdown enough:\n%s", text)
	}
	if strings.Contains(text, "Signal Lab: {") || strings.Contains(text, "Cost/Liquidation Heatmap: {") {
		t.Fatalf("formatted analysis still includes raw inline JSON:\n%s", text)
	}
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
