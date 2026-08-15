package signal

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

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
		"AAA":      0.45 * m,  // +m price, 0 funding, 0 oidelta
		"BBB":      -0.15 * m, // 0 price, -m funding, +m oidelta
		"CCC":      -0.30 * m, // -m price, +m funding, -m oidelta
		"xyz:NVDA": 0,         // std=0 cohort -> all z=0
	}
	wantBias := map[string]string{
		"AAA": "bullish", "BBB": "bearish", "CCC": "bearish", "xyz:NVDA": "neutral",
	}
	maxAbs := 0.45 * m

	// Rank order by |composite| descending: AAA, CCC, BBB, xyz:NVDA.
	wantRank := map[string]int{"AAA": 1, "CCC": 2, "BBB": 3, "xyz:NVDA": 4}
	wantConf := map[string]float64{
		"AAA":      1.0,
		"CCC":      (0.30 * m) / maxAbs,
		"BBB":      (0.15 * m) / maxAbs,
		"xyz:NVDA": 0.0,
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
	// Mark=100, PrevDay=100 -> move=0 -> pct=1% -> binStep=1.0. Positive
	// funding gives a mild long tilt but BOTH sides keep cost (near-balanced).
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

	data, _ := payload["data"].(map[string]interface{})
	binStep, _ := data["binStep"].(float64)
	if !approx(binStep, 1.0, 1e-9) {
		t.Errorf("binStep = %v, want 1.0 (1%% of mark)", binStep)
	}
	bins, _ := data["bins"].([]interface{})
	if len(bins) != 2*heatmapHalfRange+1 {
		t.Fatalf("bins length = %d, want %d (±%d around mark)", len(bins), 2*heatmapHalfRange+1, heatmapHalfRange)
	}

	requiredKeys := []string{
		"bucketStartPrice", "bucketEndPrice", "px", "longCost", "shortCost", "longLiq", "shortLiq",
	}
	oiUSD := a.OI * a.Mark // 1000 USD pool
	fundTilt := math.Min(1, math.Max(-1, a.Funding/0.002))
	longFrac := math.Min(0.55, math.Max(0.45, 0.5+0.05*fundTilt))
	longPool := oiUSD * longFrac
	shortPool := oiUSD * (1 - longFrac)

	// Cost weight: a mid-band cluster around mark, EXACTLY 0 at/outside it.
	costW := func(d float64) float64 {
		ad := math.Abs(d)
		if ad >= float64(heatmapMidBand) {
			return 0
		}
		return 1 - (ad/float64(heatmapMidBand))*(ad/float64(heatmapMidBand))
	}
	totalW := 0.0
	for d := -heatmapHalfRange; d <= heatmapHalfRange; d++ {
		totalW += costW(float64(d))
	}
	wantCenterLong := longPool * costW(0) / totalW
	wantCenterShort := shortPool * costW(0) / totalW

	var centerLong, centerShort float64
	markIdx := heatmapHalfRange
	markBinFound := false
	for i, b := range bins {
		bin, _ := b.(map[string]interface{})
		for _, k := range requiredKeys {
			if _, ok := bin[k]; !ok {
				t.Fatalf("bin[%d] missing required key %q (keys=%v)", i, k, bin)
			}
		}
		if i == markIdx {
			centerLong, _ = bin["longCost"].(float64)
			centerShort, _ = bin["shortCost"].(float64)
			px, _ := bin["px"].(float64)
			if !approx(px, 100.5, 1e-6) {
				t.Errorf("mark bin px = %v, want 100.5", px)
			}
			markBinFound = true
		}
	}
	if !markBinFound {
		t.Fatalf("mark bin (index %d) not found", markIdx)
	}

	// BOTH sides carry cost at the mark bin; positive funding favours long.
	if !approx(centerLong, wantCenterLong, 0.01) {
		t.Errorf("mark-bin longCost = %.4f, want %.4f (longPool/totalW)", centerLong, wantCenterLong)
	}
	if !approx(centerShort, wantCenterShort, 0.01) {
		t.Errorf("mark-bin shortCost = %.4f, want %.4f (shortPool/totalW)", centerShort, wantCenterShort)
	}
	if centerLong <= 0 || centerShort <= 0 {
		t.Errorf("both sides must hold cost, got longCost=%v shortCost=%v", centerLong, centerShort)
	}
	if centerLong <= centerShort {
		t.Errorf("positive funding should tilt cost toward long, got long=%v short=%v", centerLong, centerShort)
	}
	// Mild tilt: the two sides stay near-balanced like claw402's ~53M/51M.
	if centerLong > centerShort*1.5 || centerShort > centerLong*1.5 {
		t.Errorf("cost should be near-balanced, got long=%v short=%v", centerLong, centerShort)
	}
	// USD-scale sanity: cost is meaningful USD, never a raw base-coin notional.
	if centerLong <= 0 || math.IsInf(centerLong, 0) || math.IsNaN(centerLong) {
		t.Errorf("longCost not a sane USD value: %v", centerLong)
	}

	// costAddrs/liqAddrs are plausible non-zero proxies, not 0.
	costAddrs, _ := data["costAddrs"].(float64)
	liqAddrs, _ := data["liqAddrs"].(float64)
	if costAddrs <= 0 || liqAddrs <= 0 {
		t.Errorf("costAddrs/liqAddrs should be non-zero, got %v / %v", costAddrs, liqAddrs)
	}
}

func TestHeatmapRealPatternCostClusterLiqFanout(t *testing.T) {
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
	data, _ := payload["data"].(map[string]interface{})
	bins, _ := data["bins"].([]interface{})
	markIdx := heatmapHalfRange

	// Collect per-bin longCost/longLiq/shortCost/shortLiq.
	type row struct{ lc, ll, sc, sl float64 }
	rows := make([]row, len(bins))
	for i, b := range bins {
		bin, _ := b.(map[string]interface{})
		rows[i] = row{
			lc: bin["longCost"].(float64),
			ll: bin["longLiq"].(float64),
			sc: bin["shortCost"].(float64),
			sl: bin["shortLiq"].(float64),
		}
	}

	// (b) Cost lives ONLY in the MID band (|d| < midBand): present (both sides,
	// near-balanced) there and EXACTLY 0 in the far-low and far-high bins.
	maxLong, maxLongIdx := rows[0].lc, 0
	for i, r := range rows {
		if r.lc > maxLong {
			maxLong, maxLongIdx = r.lc, i
		}
	}
	if maxLongIdx != markIdx {
		t.Errorf("cost cluster peak at bin %d, want mark bin %d", maxLongIdx, markIdx)
	}
	// Cost decays within the band and is exactly 0 at/outside its boundary.
	for d := 1; d < heatmapMidBand; d++ {
		if rows[markIdx+d].lc >= rows[markIdx+d-1].lc || rows[markIdx-d].lc >= rows[markIdx-d+1].lc {
			t.Errorf("cost should decay away from mark within the band at d=%d", d)
		}
	}
	for d := heatmapMidBand; d <= heatmapHalfRange; d++ {
		if rows[markIdx+d].lc != 0 || rows[markIdx+d].sc != 0 {
			t.Errorf("far-high bin d=%d must have cost EXACTLY 0, got long=%v short=%v", d, rows[markIdx+d].lc, rows[markIdx+d].sc)
		}
		if rows[markIdx-d].lc != 0 || rows[markIdx-d].sc != 0 {
			t.Errorf("far-low bin d=%d must have cost EXACTLY 0, got long=%v short=%v", d, rows[markIdx-d].lc, rows[markIdx-d].sc)
		}
	}
	// Both cost sides present and near-balanced in the band.
	for d := 0; d < heatmapMidBand; d++ {
		lc, sc := rows[markIdx-d].lc, rows[markIdx-d].sc
		if lc <= 0 || sc <= 0 {
			t.Errorf("mid-band bin d=%d must have cost on both sides, got long=%v short=%v", d, lc, sc)
		}
		if lc > sc*1.5 || sc > lc*1.5 {
			t.Errorf("mid-band cost should be near-balanced at d=%d, got long=%v short=%v", d, lc, sc)
		}
	}

	// (c) Liquidation fans out far: long-liq ONLY below mark, short-liq ONLY
	// above mark, present on every side bin — so far-low bins are longLiq-only
	// and far-high bins are shortLiq-only (cost already verified 0 there).
	belowLongLiq, aboveShortLiq := 0, 0
	peakLongIdx, peakShortIdx := -1, -1
	for i, r := range rows {
		switch {
		case i < markIdx:
			if r.sl != 0 {
				t.Errorf("bin %d (below mark) must have no shortLiq, got %v", i, r.sl)
			}
			if r.ll > 0 {
				belowLongLiq++
				if peakLongIdx < 0 || r.ll > rows[peakLongIdx].ll {
					peakLongIdx = i
				}
			}
		case i > markIdx:
			if r.ll != 0 {
				t.Errorf("bin %d (above mark) must have no longLiq, got %v", i, r.ll)
			}
			if r.sl > 0 {
				aboveShortLiq++
				if peakShortIdx < 0 || r.sl > rows[peakShortIdx].sl {
					peakShortIdx = i
				}
			}
		default:
			if r.ll != 0 || r.sl != 0 {
				t.Errorf("mark bin must have no liq, got longLiq=%v shortLiq=%v", r.ll, r.sl)
			}
		}
	}
	if belowLongLiq < heatmapHalfRange-1 {
		t.Errorf("long-liq should fan across every below-mark bin, got %d", belowLongLiq)
	}
	if aboveShortLiq < heatmapHalfRange-1 {
		t.Errorf("short-liq should fan across every above-mark bin, got %d", aboveShortLiq)
	}
	if peakLongIdx < 0 || peakLongIdx >= markIdx {
		t.Errorf("long-liq peak should be below mark, got %d", peakLongIdx)
	}
	if peakShortIdx < 0 || peakShortIdx <= markIdx {
		t.Errorf("short-liq peak should be above mark, got %d", peakShortIdx)
	}

	// (d) Liquidation prominence: the liq peak is a substantial fraction of the
	// mark cost (claw402 ~33%; target 15%..50%) so the map is not cost-dominated.
	markCost := rows[markIdx].lc
	peakLiq := rows[peakLongIdx].ll
	if peakLiq <= 0 || peakLiq < 0.15*markCost || peakLiq > 0.5*markCost {
		t.Errorf("long-liq peak %.4f should be 15%%..50%% of mark cost %.4f (ratio %.3f)",
			peakLiq, markCost, peakLiq/markCost)
	}

	// (e) Cost and liq CO-OCCUR in the mid band (as real claw402 data does).
	cooccur := 0
	for _, r := range rows {
		if r.lc > 0 && r.ll > 0 {
			cooccur++
		}
	}
	if cooccur == 0 {
		t.Error("expected cost and liq to co-occur in some bins (no strict separation)")
	}

	// (e) Ladder fully populated: every row has at least one non-zero metric,
	// so the frontend row-filter leaves no gap rows.
	for i, r := range rows {
		if r.lc == 0 && r.ll == 0 && r.sc == 0 && r.sl == 0 {
			t.Errorf("bin %d is empty (gap row)", i)
		}
	}

	// Deterministic: two calls produce byte-identical output.
	raw2, err := s.Heatmap("BTC")
	if err != nil {
		t.Fatalf("Heatmap error (2nd call): %v", err)
	}
	if string(raw) != string(raw2) {
		t.Error("Heatmap output is not deterministic across calls")
	}
}

func TestHeatmapNegativeFundingTiltsShortSide(t *testing.T) {
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: -0.001, OI: 10}
	s := testService([]*asset{a})
	raw, err := s.Heatmap("BTC")
	if err != nil {
		t.Fatalf("Heatmap error: %v", err)
	}
	var payload map[string]interface{}
	_ = json.Unmarshal(raw, &payload)
	data, _ := payload["data"].(map[string]interface{})
	bins, _ := data["bins"].([]interface{})
	bin, _ := bins[heatmapHalfRange].(map[string]interface{})
	longCost, _ := bin["longCost"].(float64)
	shortCost, _ := bin["shortCost"].(float64)
	if longCost <= 0 {
		t.Errorf("negative funding must still leave cost on the long side, got longCost=%v", longCost)
	}
	if shortCost <= 0 {
		t.Errorf("short side should hold cost, got shortCost=%v", shortCost)
	}
	if shortCost <= longCost {
		t.Errorf("negative funding should tilt cost toward short, got long=%v short=%v", longCost, shortCost)
	}
}

func TestAssetForResolvesXYZFallback(t *testing.T) {
	// Seed the snapshot with: a bare crypto asset (2Z), an already-prefixed xyz
	// asset (xyz:SP500, xyz:USAR), and a newer xyz asset (xyz:NBIS) that is NOT
	// in the hardcoded provider lists. assetFor must resolve bare "NBIS" to the
	// stored xyz:NBIS via the xyz:<base> fallback, while crypto and already-
	// prefixed symbols resolve unchanged.
	assets := []*asset{
		{Symbol: "2Z", MarketType: "core_perp", Category: "crypto",
			Mark: 100, PrevDay: 100, Funding: 0.001, OI: 10},
		{Symbol: "xyz:SP500", MarketType: "hip3_perp", Category: "Equities",
			Mark: 5000, PrevDay: 5000, Funding: 0.0, OI: 1},
		{Symbol: "xyz:USAR", MarketType: "hip3_perp", Category: "Forex",
			Mark: 1.2, PrevDay: 1.2, Funding: 0.0, OI: 1},
		{Symbol: "xyz:NBIS", MarketType: "hip3_perp", Category: "Equities",
			Mark: 30, PrevDay: 30, Funding: 0.0, OI: 1},
	}
	s := testService(assets)

	cases := []struct {
		input string
		want  string
	}{
		{"NBIS", "xyz:NBIS"},       // stale-list asset, bare form -> xyz: fallback
		{"2Z", "2Z"},               // crypto bare symbol -> unchanged
		{"xyz:SP500", "xyz:SP500"}, // already-prefixed -> unchanged
		{"xyz:USAR", "xyz:USAR"},   // already-prefixed -> unchanged
	}
	for _, tc := range cases {
		a, ok := s.assetFor(tc.input)
		if !ok {
			t.Errorf("assetFor(%q) not found", tc.input)
			continue
		}
		if a.Symbol != tc.want {
			t.Errorf("assetFor(%q) resolved to %q, want %q", tc.input, a.Symbol, tc.want)
		}
	}

	// Unknown symbols still fail.
	if _, ok := s.assetFor("ETH"); ok {
		t.Error("assetFor(ETH) should not resolve")
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

// ---------------------------------------------------------------------------
// F1: Rank must not mutate shared snapshot pointers; SignalLab recomputes the
// per-symbol cohort composite on demand (matches Rank's value).
// ---------------------------------------------------------------------------

func TestRankDoesNotMutateSharedScore_AndSignalLabRecomputes(t *testing.T) {
	s := testService(buildRankFixture())
	board := s.Rank(0)
	item := itemBySymbol(board.Items, "AAA")
	if item == nil {
		t.Fatal("AAA missing from ranking")
	}
	// F1: Rank must NOT write the composite onto the shared asset pointer.
	if got := s.assets["AAA"].Score; got != 0 {
		t.Errorf("Rank mutated asset.Score = %v, want 0 (must not write shared pointers)", got)
	}

	body, err := s.SignalLab("AAA")
	if err != nil {
		t.Fatalf("SignalLab error: %v", err)
	}
	var out struct {
		Data struct {
			CompositeZ string `json:"compositeZ"`
			Score      string `json:"score"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode SignalLab: %v", err)
	}
	if out.Data.CompositeZ == "" {
		t.Fatalf("compositeZ not emitted by SignalLab recompute")
	}
	got, err := strconv.ParseFloat(out.Data.CompositeZ, 64)
	if err != nil {
		t.Fatalf("parse compositeZ %q: %v", out.Data.CompositeZ, err)
	}
	if !approx(got, item.Score, 1e-6) {
		t.Errorf("SignalLab recomputed compositeZ = %v, want %v (matches Rank)", got, item.Score)
	}
	if out.Data.Score != out.Data.CompositeZ {
		t.Errorf("score scalar = %q, want compositeZ %q", out.Data.Score, out.Data.CompositeZ)
	}
}

// ---------------------------------------------------------------------------
// Real WS taker-flow + order-book -> Signal Lab Flow/Liquidity rows
// ---------------------------------------------------------------------------

// seedFlowFreezesNow seeds the service's WS flow accumulator for a symbol and
// freezes s.now() to the given timestamp so decay is deterministic in tests.
func seedFlow(s *Service, sym string, now time.Time, buy, sell map[int]flowBin, bidTotal, askTotal float64) {
	s.mu.Lock()
	s.flow[sym] = &SymbolFlow{
		Coin:       sym,
		anchorMark: 100,
		anchorStep: 1,
		buyByBin:   buy,
		sellByBin:  sell,
		bidTotal:   bidTotal,
		askTotal:   askTotal,
	}
	s.now = func() time.Time { return now }
	s.mu.Unlock()
}

// labDimRows decodes the SignalLab payload and returns the dimensions[] rows.
func labDimRows(t *testing.T, raw json.RawMessage) []map[string]interface{} {
	t.Helper()
	var out struct {
		Data struct {
			Dimensions []map[string]interface{} `json:"dimensions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode SignalLab: %v", err)
	}
	return out.Data.Dimensions
}

func TestSignalLabTakerFlowBuyHeavy(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	// Buy-heavy: 10000$ buys vs 2000$ sells -> ratio 0.833 (>=0.55 bullish).
	buy := map[int]flowBin{0: {notional: 10000, lastTouch: now}}
	sell := map[int]flowBin{0: {notional: 2000, lastTouch: now}}
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	seedFlow(s, "BTC", now, buy, sell, 0, 0)

	rows := labDimRows(t, mustLab(s, "BTC"))
	row := findDim(rows, "Flow", "Taker Flow")
	if row == nil {
		t.Fatalf("Taker Flow row missing; rows=%v", dimLabels(rows))
	}
	if row["direction"] != "bullish" {
		t.Errorf("direction = %v, want bullish", row["direction"])
	}
	if row["strength"] != "strong" {
		t.Errorf("strength = %v, want strong (|0.833-0.5|*2=0.667)", row["strength"])
	}
	// Rows are prepended: Flow row must be first.
	if rows[0]["family"] != "Flow" {
		t.Errorf("Flow row not first; first=%v", rows[0]["family"])
	}
}

func TestSignalLabTakerFlowSellHeavy(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	// Sell-heavy: 1000$ buys vs 9000$ sells -> ratio 0.10 (<=0.45 bearish).
	buy := map[int]flowBin{0: {notional: 1000, lastTouch: now}}
	sell := map[int]flowBin{0: {notional: 9000, lastTouch: now}}
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	seedFlow(s, "BTC", now, buy, sell, 0, 0)

	rows := labDimRows(t, mustLab(s, "BTC"))
	row := findDim(rows, "Flow", "Taker Flow")
	if row == nil {
		t.Fatalf("Taker Flow row missing; rows=%v", dimLabels(rows))
	}
	if row["direction"] != "bearish" {
		t.Errorf("direction = %v, want bearish", row["direction"])
	}
}

func TestSignalLabBookImbalanceSkewed(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	// Bid-skewed: bid 8000 vs ask 2000 -> imb +0.60 (>+0.10 bullish, |imb|>=0.4 strong).
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	seedFlow(s, "BTC", now, nil, nil, 8000, 2000)

	rows := labDimRows(t, mustLab(s, "BTC"))
	row := findDim(rows, "Liquidity", "Order-Book Imbalance")
	if row == nil {
		t.Fatalf("Book Imbalance row missing; rows=%v", dimLabels(rows))
	}
	if row["direction"] != "bullish" {
		t.Errorf("direction = %v, want bullish", row["direction"])
	}
	if row["strength"] != "strong" {
		t.Errorf("strength = %v, want strong", row["strength"])
	}
}

func TestSignalLabFlowOmittedWhenNil(t *testing.T) {
	// No WS flow at all -> flow rows must be OMITTED (never zero-as-balanced).
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	rows := labDimRows(t, mustLab(s, "BTC"))
	if findDim(rows, "Flow", "Taker Flow") != nil || findDim(rows, "Liquidity", "Order-Book Imbalance") != nil {
		t.Errorf("flow rows must be omitted when s.flow is nil; rows=%v", dimLabels(rows))
	}
	if len(rows) != 3 {
		t.Errorf("only 3 core rows expected, got %d", len(rows))
	}
}

func TestSignalLabFlowOmittedWhenAllDecayed(t *testing.T) {
	// Flow exists but is stale (lastTouch well beyond FlowMaxAge=2h) -> decayedBin
	// returns 0 -> totF==0 -> rows omitted.
	old := time.Date(2026, 8, 15, 6, 0, 0, 0, time.UTC) // 6h before now
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	buy := map[int]flowBin{0: {notional: 10000, lastTouch: old}}
	sell := map[int]flowBin{0: {notional: 2000, lastTouch: old}}
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	seedFlow(s, "BTC", now, buy, sell, 0, 0)

	rows := labDimRows(t, mustLab(s, "BTC"))
	if findDim(rows, "Flow", "Taker Flow") != nil {
		t.Errorf("stale Taker Flow must be omitted; rows=%v", dimLabels(rows))
	}
}

func TestSignalLabFlowRowsOrderingAndCap(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	buy := map[int]flowBin{0: {notional: 6000, lastTouch: now}}
	sell := map[int]flowBin{0: {notional: 4000, lastTouch: now}}
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	seedFlow(s, "BTC", now, buy, sell, 5000, 5000)

	rows := labDimRows(t, mustLab(s, "BTC"))
	// Flow-first, then core rows; never exceed the formatter's 8-row cap.
	if rows[0]["family"] != "Flow" || rows[1]["family"] != "Liquidity" {
		t.Errorf("flow rows not first two; rows=%v", dimLabels(rows))
	}
	if len(rows) > 8 {
		t.Errorf("rows exceed 8-row cap: %d", len(rows))
	}
}

func TestSignalLabFlowDeterministicJSON(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	buy := map[int]flowBin{0: {notional: 6000, lastTouch: now}, 3: {notional: 2000, lastTouch: now}}
	sell := map[int]flowBin{0: {notional: 2000, lastTouch: now}, -2: {notional: 1000, lastTouch: now}}
	a := &asset{Symbol: "BTC", MarketType: "core_perp", Category: "crypto",
		Mark: 100, PrevDay: 100, Funding: 0.0, OI: 10}
	s := testService([]*asset{a})
	seedFlow(s, "BTC", now, buy, sell, 7000, 3000)

	r1 := string(mustLab(s, "BTC"))
	r2 := string(mustLab(s, "BTC"))
	if r1 != r2 {
		t.Error("SignalLab output not deterministic across two calls")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustLab(s *Service, sym string) json.RawMessage {
	body, err := s.SignalLab(sym)
	if err != nil {
		panic("SignalLab error: " + err.Error())
	}
	return body
}

func findDim(rows []map[string]interface{}, family, label string) map[string]interface{} {
	for _, r := range rows {
		if r["family"] == family && r["label"] == label {
			return r
		}
	}
	return nil
}

func dimLabels(rows []map[string]interface{}) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["family"].(string)+"/"+r["label"].(string))
	}
	return out
}
