package signal

import (
	"encoding/json"
	"math"
	"testing"

	"nofx/provider/vergex"
)

// testService builds a Service whose snapshot is seeded directly from the
// given assets (no network / no Hyperliquid ingest), with a stable ordering.
func testService(assets []*asset) *Service {
	s := NewService(&Config{})
	m := make(map[string]*asset, len(assets))
	order := make([]string, 0, len(assets))
	for _, a := range assets {
		m[a.Symbol] = a
		order = append(order, a.Symbol)
	}
	s.mu.Lock()
	s.assets = m
	s.order = order
	s.mu.Unlock()
	return s
}

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func itemBySymbol(items []vergex.SignalRankItem, sym string) *vergex.SignalRankItem {
	for i := range items {
		if items[i].Symbol == sym {
			return &items[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rank
// ---------------------------------------------------------------------------

// buildRankFixture returns three core_perp assets engineered so every factor's
// z-score magnitude is math.Sqrt(1.5) (= m below), producing UNIQUE composite
// magnitudes so the rank order is deterministic (no sort ties, no reliance on
// map iteration order), plus one single-asset hip3_perp cohort whose std=0
// forces all its z-scores to 0.
func buildRankFixture() []*asset {
	return []*asset{
		// AAA: price24h high, funding mid, oidelta low -> strongest |composite|.
		{Symbol: "AAA", MarketType: "core_perp", Category: "crypto",
			Mark: 130, PrevDay: 100, Funding: 0.0010, OI: 6, OIPrev: 4},
		// BBB: price mid, funding low, oidelta high.
		{Symbol: "BBB", MarketType: "core_perp", Category: "crypto",
			Mark: 120, PrevDay: 100, Funding: 0.0005, OI: 8, OIPrev: 5},
		// CCC: price low, funding high, oidelta low.
		{Symbol: "CCC", MarketType: "core_perp", Category: "crypto",
			Mark: 110, PrevDay: 100, Funding: 0.0015, OI: 5, OIPrev: 4},
		// xyz:NVDA: single hip3_perp cohort -> std=0 -> all z=0, neutral.
		{Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "Technology",
			Mark: 50, PrevDay: 50, Funding: 0.0005, OI: 100, OIPrev: 100},
	}
}

func TestRankDeterministicCompositeConfidenceBias(t *testing.T) {
	s := testService(buildRankFixture())
	data := s.Rank(0)
	if len(data.Items) != 4 {
		t.Fatalf("Rank returned %d items, want 4", len(data.Items))
	}

	// Every non-constant factor in this fixture has z-magnitude = sqrt(1.5).
	m := math.Sqrt(1.5)
	wantScore := map[string]float64{
		"AAA":       0.45 * m,                // +m price, 0 funding, 0 oidelta
		"BBB":      -0.15 * m,                // 0 price, -m funding, +m oidelta
		"CCC":      -0.30 * m,                // -m price, +m funding, -m oidelta
		"xyz:NVDA": 0,                        // std=0 cohort -> all z=0
	}
	wantBias := map[string]string{
		"AAA": "bullish", "BBB": "bearish", "CCC": "bearish", "xyz:NVDA": "neutral",
	}
	maxAbs := 0.45 * m

	// Rank order by |composite| descending: AAA, CCC, BBB, xyz:NVDA.
	wantRank := map[string]int{"AAA": 1, "CCC": 2, "BBB": 3, "xyz:NVDA": 4}
	wantConf := map[string]float64{
		"AAA":       1.0,
		"CCC":       (0.30 * m) / maxAbs,
		"BBB":       (0.15 * m) / maxAbs,
		"xyz:NVDA":  0.0,
	}

	for sym, want := range wantScore {
		item := itemBySymbol(data.Items, sym)
		if item == nil {
			t.Fatalf("item %q missing from ranking", sym)
		}
		if !approx(item.Score, want, 1e-9) {
			t.Errorf("%s score = %.6f, want %.6f", sym, item.Score, want)
		}
		if item.Bias != wantBias[sym] {
			t.Errorf("%s bias = %q, want %q", sym, item.Bias, wantBias[sym])
		}
		if item.Rank != wantRank[sym] {
			t.Errorf("%s rank = %d, want %d", sym, item.Rank, wantRank[sym])
		}
		if !approx(item.Confidence, wantConf[sym], 1e-9) {
			t.Errorf("%s confidence = %.6f, want %.6f", sym, item.Confidence, wantConf[sym])
		}
		if item.MarketType == "" || item.Category == "" {
			t.Errorf("%s missing marketType/category", sym)
		}
	}

	// Confidence must be clamped to [0,1].
	if item := itemBySymbol(data.Items, "AAA"); item.Confidence > 1 {
		t.Errorf("confidence not clamped, got %.6f", item.Confidence)
	}

	// The board must carry BOTH directions when split.
	bull, bear := 0, 0
	for _, it := range data.Items {
		switch it.Bias {
		case "bullish":
			bull++
		case "bearish":
			bear++
		}
	}
	if bull == 0 || bear == 0 {
		t.Errorf("ranking not direction-balanced: bullish=%d bearish=%d", bull, bear)
	}
}

func TestRankUniverseStdZeroAllZeros(t *testing.T) {
	// All symbols identical -> every factor std=0 -> every z and composite = 0.
	assets := []*asset{
		{Symbol: "AAA", MarketType: "core_perp", Category: "crypto", Mark: 100, PrevDay: 100, Funding: 0.001, OI: 10, OIPrev: 10},
		{Symbol: "BBB", MarketType: "core_perp", Category: "crypto", Mark: 100, PrevDay: 100, Funding: 0.001, OI: 10, OIPrev: 10},
	}
	s := testService(assets)
	data := s.Rank(0)
	if len(data.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(data.Items))
	}
	for _, it := range data.Items {
		if it.Bias != "neutral" {
			t.Errorf("%s bias = %q, want neutral (std=0)", it.Symbol, it.Bias)
		}
		if it.Score != 0 || it.Confidence != 0 {
			t.Errorf("%s expected zero score/confidence on std=0, got score=%v conf=%v", it.Symbol, it.Score, it.Confidence)
		}
	}
}

// ---------------------------------------------------------------------------
// Heatmap
// ---------------------------------------------------------------------------

func TestHeatmapUSDScaleAndSchema(t *testing.T) {
	// Mark=100, PrevDay=100 -> move=0 -> pct=0.1% -> binStep=0.1. Positive
	// funding -> long side gets the whole OI*mark pool; short side is zero.
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.001, OI: 10}
	s := testService([]*asset{a})

	raw, err := s.Heatmap("BTC")
	if err != nil {
		t.Fatalf("Heatmap error: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode heatmap: %v", err)
	}

	binStep, _ := payload["binStep"].(float64)
	if !approx(binStep, 0.1, 1e-9) {
		t.Errorf("binStep = %v, want 0.1 (0.1%% of mark)", binStep)
	}
	bins, _ := payload["bins"].([]interface{})
	if len(bins) != 13 {
		t.Fatalf("bins length = %d, want 13 (±6 around mark)", len(bins))
	}

	requiredKeys := []string{
		"bucketStartPrice", "bucketEndPrice", "px", "longCost", "shortCost", "longLiq", "shortLiq",
	}
	// The mark bin is index 6 (dist 0). Its weight is 1.0.
	totalW := 1.0
	for d := 1; d <= 6; d++ {
		totalW += 2.0 / (float64(d) * float64(d))
	}
	oiUSD := a.OI * a.Mark // 1000 USD pool
	wantCenterLong := oiUSD / totalW

	var centerCost, centerLiq, centerShort float64
	markBinFound := false
	for i, b := range bins {
		bin, _ := b.(map[string]interface{})
		for _, k := range requiredKeys {
			if _, ok := bin[k]; !ok {
				t.Fatalf("bin[%d] missing required key %q (keys=%v)", i, k, bin)
			}
		}
		if i == 6 {
			centerCost, _ = bin["longCost"].(float64)
			centerShort, _ = bin["shortCost"].(float64)
			centerLiq, _ = bin["longLiq"].(float64)
			px, _ := bin["px"].(float64)
			if !approx(px, 100.05, 1e-6) {
				t.Errorf("mark bin px = %v, want 100.05", px)
			}
			markBinFound = true
		}
	}
	if !markBinFound {
		t.Fatal("mark bin (index 6) not found")
	}

	// USD-scale: positive funding -> longCost holds the pool, shortCost is 0.
	if !approx(centerCost, wantCenterLong, 0.01) {
		t.Errorf("mark-bin longCost = %.4f, want %.4f (USD pool %v/totalW)", centerCost, wantCenterLong, oiUSD)
	}
	if centerShort != 0 {
		t.Errorf("mark-bin shortCost = %v, want 0 (positive funding)", centerShort)
	}
	// longLiq is the cost-spike proxy fraction.
	if !approx(centerLiq, centerCost*0.2, 0.01) {
		t.Errorf("mark-bin longLiq = %.4f, want %.4f (0.2× longCost)", centerLiq, centerCost*0.2)
	}
	// USD-scale sanity: a positive funding pool must render meaningful USD
	// (> 0 and finite), never a raw base-coin notional.
	if centerCost <= 0 || math.IsInf(centerCost, 0) || math.IsNaN(centerCost) {
		t.Errorf("longCost not a sane USD value: %v", centerCost)
	}
}

func TestHeatmapNegativeFundingFlipsToShortSide(t *testing.T) {
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: -0.001, OI: 10}
	s := testService([]*asset{a})
	raw, err := s.Heatmap("BTC")
	if err != nil {
		t.Fatalf("Heatmap error: %v", err)
	}
	var payload map[string]interface{}
	_ = json.Unmarshal(raw, &payload)
	bins, _ := payload["bins"].([]interface{})
	bin, _ := bins[6].(map[string]interface{})
	longCost, _ := bin["longCost"].(float64)
	shortCost, _ := bin["shortCost"].(float64)
	if longCost != 0 {
		t.Errorf("negative funding should zero the long side, got longCost=%v", longCost)
	}
	if shortCost <= 0 {
		t.Errorf("negative funding should push the pool to the short side, got shortCost=%v", shortCost)
	}
}

func TestHeatmapUnknownSymbolReturnsError(t *testing.T) {
	s := testService([]*asset{{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.001, OI: 10}})
	if _, err := s.Heatmap("ETH"); err == nil {
		t.Fatal("expected error for unknown symbol")
	}
}

// ---------------------------------------------------------------------------
// NetFlow
// ---------------------------------------------------------------------------

func TestNetFlowInstitutionRetailSplits(t *testing.T) {
	assets := []*asset{
		{Symbol: "BTC", MarketType: "core_perp", Category: "crypto", Mark: 100, Funding: 0.001, OI: 10, OIPrev: 5},
		{Symbol: "ETH", MarketType: "core_perp", Category: "crypto", Mark: 50, Funding: -0.002, OI: 20, OIPrev: 15},
		{Symbol: "SOL", MarketType: "core_perp", Category: "crypto", Mark: 10, Funding: 0.0, OI: 100, OIPrev: 100},
	}
	s := testService(assets)
	data, err := s.NetFlow("1h", 10)
	if err != nil {
		t.Fatalf("NetFlow error: %v", err)
	}
	if data.Duration != "1h" || data.TimeRange != "1h" {
		t.Errorf("window not preserved: duration=%q timeRange=%q", data.Duration, data.TimeRange)
	}

	// Institution = funding × OI × mark.
	//   BTC: 0.001*10*100 =  1.0
	//   ETH: -0.002*20*50 = -2.0
	//   SOL: 0.0*100*10   =  0.0
	if len(data.InstitutionFutureTop) != 2 {
		t.Fatalf("InstitutionFutureTop len=%d, want 2 (BTC+SOL)", len(data.InstitutionFutureTop))
	}
	if len(data.InstitutionFutureLow) != 1 {
		t.Fatalf("InstitutionFutureLow len=%d, want 1 (ETH)", len(data.InstitutionFutureLow))
	}
	top := data.InstitutionFutureTop
	if top[0].Symbol != "BTC" || !approx(top[0].Amount, 1.0, 0.01) || top[0].Rank != 1 {
		t.Errorf("InstitutionFutureTop[0] = %+v, want BTC/1.0/rank1", top[0])
	}
	if top[1].Symbol != "SOL" || !approx(top[1].Amount, 0.0, 0.01) || top[1].Rank != 2 {
		t.Errorf("InstitutionFutureTop[1] = %+v, want SOL/0.0/rank2", top[1])
	}
	low := data.InstitutionFutureLow
	if low[0].Symbol != "ETH" || !approx(low[0].Amount, -2.0, 0.01) || low[0].Rank != 1 {
		t.Errorf("InstitutionFutureLow[0] = %+v, want ETH/-2.0/rank1", low[0])
	}

	// Retail = OI-delta × mark.
	//   BTC: 5*100 = 500
	//   ETH: 5*50  = 250
	//   SOL: 0*10  = 0
	// Retail = OI-delta × mark. ETH's OI-delta (5×50=250) is positive, so it
	// joins the retail TOP feed alongside BTC (500) and SOL (0).
	if len(data.PersonalFutureTop) != 3 {
		t.Fatalf("PersonalFutureTop len=%d, want 3 (BTC, ETH, SOL)", len(data.PersonalFutureTop))
	}
	ptop := data.PersonalFutureTop
	if ptop[0].Symbol != "BTC" || !approx(ptop[0].Amount, 500.0, 0.01) || ptop[0].Rank != 1 {
		t.Errorf("PersonalFutureTop[0] = %+v, want BTC/500/rank1", ptop[0])
	}
	if ptop[1].Symbol != "ETH" || !approx(ptop[1].Amount, 250.0, 0.01) || ptop[1].Rank != 2 {
		t.Errorf("PersonalFutureTop[1] = %+v, want ETH/250/rank2", ptop[1])
	}
	if ptop[2].Symbol != "SOL" || ptop[2].Rank != 3 {
		t.Errorf("PersonalFutureTop[2] = %+v, want SOL/rank3", ptop[2])
	}
	if len(data.PersonalFutureLow) != 0 {
		t.Errorf("PersonalFutureLow len=%d, want 0 (no negative OI-delta)", len(data.PersonalFutureLow))
	}
}

// TestNetFlowColdStart verifies the OIPrev=0 cold-start rule: on the first
// poll there is no prior OI, so OI-delta is undefined and the retail
// (PersonalFuture*) feed stays empty; it fills only after a second poll
// establishes a prior OI.
func TestNetFlowColdStartOIPrevZero(t *testing.T) {
	mk := func(oiprev float64) *asset {
		return &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
			Mark: 100, Funding: 0.001, OI: 10, OIPrev: oiprev}
	}
	// First poll: OIPrev=0 -> no retail entries.
	s := testService([]*asset{mk(0)})
	data, err := s.NetFlow("1h", 10)
	if err != nil {
		t.Fatalf("NetFlow error: %v", err)
	}
	if len(data.PersonalFutureTop) != 0 || len(data.PersonalFutureLow) != 0 {
		t.Errorf("cold-start should have empty PersonalFuture feeds, got top=%d low=%d",
			len(data.PersonalFutureTop), len(data.PersonalFutureLow))
	}

	// Second poll: a prior OI exists -> OI-delta (10-5=5) is real.
	s2 := testService([]*asset{mk(5)})
	data2, err := s2.NetFlow("1h", 10)
	if err != nil {
		t.Fatalf("NetFlow error: %v", err)
	}
	if len(data2.PersonalFutureTop) != 1 {
		t.Fatalf("2nd-poll PersonalFutureTop len=%d, want 1", len(data2.PersonalFutureTop))
	}
	if !approx(data2.PersonalFutureTop[0].Amount, 5*100, 0.01) {
		t.Errorf("PersonalFutureTop[0].Amount = %v, want 500", data2.PersonalFutureTop[0].Amount)
	}
}

// Ensure the computed ranking round-trips through the shared vergex contract
// (shape symmetry used by the engine).
func TestRankMarshalsAsSignalRankingContract(t *testing.T) {
	s := testService(buildRankFixture())
	data := s.Rank(0)
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal SignalRankingData: %v", err)
	}
	var parsed vergex.SignalRankingData
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal SignalRankingData: %v", err)
	}
	if len(parsed.Items) != 4 {
		t.Errorf("round-trip items = %d, want 4", len(parsed.Items))
	}
	if parsed.Items[0].Bias == "" || parsed.Items[0].Symbol == "" {
		t.Errorf("round-trip item lost bias/symbol: %+v", parsed.Items[0])
	}
}
