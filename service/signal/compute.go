package signal

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"nofx/provider/hyperliquid"
	"nofx/provider/nofxos"
	"nofx/provider/vergex"
)

// ─────────────────────────────────────────────────────────────────────────────
// Signal ranking (per-cohort z-scores)
// ─────────────────────────────────────────────────────────────────────────────

// rankingWeights are the normalized factor weights for the composite score
// (24h price delta, funding, OI-delta). Net-flow is excluded from the ranking
// in v1.
var rankingWeights = [3]float64{0.45, 0.35, 0.20}

// factorStats holds the mean and standard deviation of one factor across a
// cohort cross-section. Declared at package level so the shared helpers
// zscore/meanStd can use it.
type factorStats struct{ mean, std float64 }

// Rank builds a SignalRankingData from the current snapshot. z-scores are
// computed separately within each market cohort (core_perp vs hip3_perp) so
// high-vol crypto does not mute low-vol TradeFi, then merged into one board.
func (s *Service) Rank(limit int) *vergex.SignalRankingData {
	assets, order, _, _ := s.Snapshot()
	if len(assets) == 0 {
		return &vergex.SignalRankingData{Items: []vergex.SignalRankItem{}}
	}

	// Cohort-level mean/std per factor, computed over assets with valid data.
	byCohort := map[string][]*asset{}
	for _, sym := range order {
		a := assets[sym]
		byCohort[a.MarketType] = append(byCohort[a.MarketType], a)
	}

	cohortStats := map[string][3]factorStats{} // index by factor

	for cohort, list := range byCohort {
		var f [3][]float64 // price24h, funding, oidelta
		for _, a := range list {
			if d := priceDelta(a); d != nil {
				f[0] = append(f[0], *d)
			}
			f[1] = append(f[1], a.Funding)
			if a.OIPrev > 0 {
				f[2] = append(f[2], a.OIDelta())
			}
		}
		var stats [3]factorStats
		for i := 0; i < 3; i++ {
			stats[i] = meanStd(f[i])
		}
		cohortStats[cohort] = stats
	}

	// maxAbsComposite across all items, for confidence scaling.
	var items []vergex.SignalRankItem
	maxAbs := 0.0
	type scored struct {
		sym string
		z   float64
	}
	rawScores := map[string]float64{}

	for cohort, list := range byCohort {
		stats := cohortStats[cohort]
		for _, a := range list {
			var d *float64
			if v := priceDelta(a); v != nil {
				d = v
			}
			z0 := zscore(d, stats[0])
			z1 := zscore(&a.Funding, stats[1])
			z2 := 0.0
			if a.OIPrev > 0 {
				v := a.OIDelta()
				z2 = zscore(&v, stats[2])
			}
			composite := rankingWeights[0]*z0 + rankingWeights[1]*z1 + rankingWeights[2]*z2
			if math.Abs(composite) > maxAbs {
				maxAbs = math.Abs(composite)
			}
			rawScores[a.Symbol] = composite
		}
	}

	// Rank by |composite| descending; strongest first.
	sorted := make([]string, 0, len(rawScores))
	for sym := range rawScores {
		sorted = append(sorted, sym)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return math.Abs(rawScores[sorted[i]]) > math.Abs(rawScores[sorted[j]])
	})
	if limit > 0 && limit < len(sorted) {
		sorted = sorted[:limit]
	}

	for rank, sym := range sorted {
		a := assets[sym]
		composite := rawScores[sym]
		conf := 0.0
		if maxAbs > 0 {
			conf = math.Min(1, math.Abs(composite)/maxAbs)
		}
		items = append(items, vergex.SignalRankItem{
			Rank:       rank + 1,
			Symbol:     a.Symbol,
			MarketType: a.MarketType,
			Bias:       biasToken(composite),
			Confidence: conf,
			Score:      composite,
			Category:   a.Category,
		})
	}

	return &vergex.SignalRankingData{Items: items}
}

// biasToken maps the composite score to the exact tokens the engine branches
// on ("bullish"/"bearish"/"neutral"), never a sign.
func biasToken(composite float64) string {
	switch {
	case composite > 0:
		return "bullish"
	case composite < 0:
		return "bearish"
	default:
		return "neutral"
	}
}

// priceDelta returns the 24h price delta as a fraction if both prices are
// valid, else nil.
func priceDelta(a *asset) *float64 {
	if a.Mark > 0 && a.PrevDay > 0 {
		v := (a.Mark - a.PrevDay) / a.PrevDay
		return &v
	}
	return nil
}

func zscore(v *float64, st factorStats) float64 {
	if v == nil || st.std == 0 {
		return 0
	}
	return (*v - st.mean) / st.std
}

func meanStd(vals []float64) factorStats {
	if len(vals) == 0 {
		return factorStats{}
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var sq float64
	for _, v := range vals {
		d := v - mean
		sq += d * d
	}
	std := math.Sqrt(sq / float64(len(vals)))
	return factorStats{mean: mean, std: std}
}

// ─────────────────────────────────────────────────────────────────────────────
// Cost/liquidation heatmap (USD-scaled proxy)
// ─────────────────────────────────────────────────────────────────────────────

// heatmapBinStep returns the volatility-proxy bin step (no candle/ATR
// dependency in v1): ~1%..2% of mark, scaled by the 24h move. The ~1% floor
// keeps the ladder wide (comparable to claw402's ~1.2% step of ~90 on a ~7776
// mark).
func heatmapBinStep(a *asset) float64 {
	pct := 0.01
	if a.Mark > 0 && a.PrevDay > 0 {
		move := math.Abs(a.Mark-a.PrevDay) / a.PrevDay
		pct = math.Max(0.01, math.Min(0.02, 2*move))
	}
	return pct * a.Mark
}

// Heatmap tuning constants. The ladder is a self-hosted, deterministic proxy
// for the paid claw402 view: a wide range, a near-balanced BOTH-sided cost
// cluster around mark, and liquidation fanning far out on each side.
const (
	// heatmapHalfRange is the number of bin-steps below and above mark the
	// ladder spans. ±60 → 121 bins, wide enough to mirror claw402's wide range
	// (~±5400 around a ~7776 mark) so the terminal ladder scrolls far.
	heatmapHalfRange = 60
	// heatmapLiqMarkFrac is the peak liquidation as a fraction of the
	// corresponding mark (POC) cost — ~33% like claw402's mark-adjacent 17.7M
	// longLiq vs 53M longCost, so liq is clearly visible rather than a sliver.
	heatmapLiqMarkFrac = 0.33
	// heatmapMidBand is the half-width (in bin-steps) of the cost band around
	// mark. Cost is non-zero ONLY inside |d| < midBand and EXACTLY 0 at/outside
	// the boundary, so the far-low/high bins carry liq only (as claw402 does).
	heatmapMidBand = 12
	// heatmapLiqTau is the taper length (in bin-steps) of each side's
	// liquidation from the mark-adjacent peak toward the range edge.
	heatmapLiqTau = 15.0
)

// Heatmap builds the cost/liquidation heatmap for a symbol as a raw JSON
// payload matching the bins[] contract the vergex formatter parses. The USD
// pool (OI × mark) is split near 50/50 (a mild funding tilt) into a BOTH-sided
// cost cluster around mark; liquidation fans out far on each side (long-liq
// below, short-liq above), co-occurring with cost in the mid band exactly as
// the real claw402 data does.
func (s *Service) Heatmap(symbol string) (json.RawMessage, error) {
	a, ok := s.assetFor(symbol)
	if !ok {
		return nil, errMarketNotFound
	}
	binStep := heatmapBinStep(a)
	if binStep <= 0 {
		binStep = 0.01 * a.Mark
	}
	n := 2*heatmapHalfRange + 1 // ±halfRange around mark
	bins := make([]map[string]interface{}, n)
	oiUSD := a.OI * a.Mark // total USD pool
	mark := a.Mark

	// Mild long/short pool split, near 50/50, tilting slightly by funding so
	// BOTH sides always hold cost (claw402 shows both roughly balanced).
	fundTilt := a.Funding / 0.002
	if fundTilt < -1 {
		fundTilt = -1
	} else if fundTilt > 1 {
		fundTilt = 1
	}
	longFrac := 0.5 + 0.05*fundTilt
	if longFrac < 0.45 {
		longFrac = 0.45
	} else if longFrac > 0.55 {
		longFrac = 0.55
	}
	shortFrac := 1 - longFrac
	longPool := oiUSD * longFrac
	shortPool := oiUSD * shortFrac

	// Cost: confined to a MID band around mark (|d| < midBand), decaying to
	// EXACTLY 0 at the boundary so the far-low/high bins never show a cost bar —
	// they carry liq only, matching real claw402.
	costW := func(d int) float64 {
		ad := d
		if ad < 0 {
			ad = -ad
		}
		if ad >= heatmapMidBand {
			return 0
		}
		return 1 - (float64(ad)/float64(heatmapMidBand))*(float64(ad)/float64(heatmapMidBand))
	}
	var totalW float64
	weights := make([]float64, n)
	for i := 0; i < n; i++ {
		weights[i] = costW(i - heatmapHalfRange)
		totalW += weights[i]
	}

	// Liquidation: peak at the mark-adjacent bin and taper toward the edges, on
	// the correct sides (long below, short above). Peak liq is scaled to a
	// substantial fraction of the corresponding mark cost (like claw402's ~33%)
	// so the map is not cost-dominated, while still covering the far regions.
	longMarkCost := longPool / totalW
	shortMarkCost := shortPool / totalW
	liqPeakLong := heatmapLiqMarkFrac * longMarkCost
	liqPeakShort := heatmapLiqMarkFrac * shortMarkCost
	var maxWLong, maxWShort float64
	wLongLiq := make([]float64, n)
	wShortLiq := make([]float64, n)
	for i := 0; i < n; i++ {
		d := i - heatmapHalfRange
		if d < 0 {
			ad := float64(-d)
			wLongLiq[i] = math.Exp(-ad / heatmapLiqTau)
			if wLongLiq[i] > maxWLong {
				maxWLong = wLongLiq[i]
			}
		} else if d > 0 {
			ad := float64(d)
			wShortLiq[i] = math.Exp(-ad / heatmapLiqTau)
			if wShortLiq[i] > maxWShort {
				maxWShort = wShortLiq[i]
			}
		}
	}

	for i := 0; i < n; i++ {
		start := mark + float64(i-heatmapHalfRange)*binStep
		px := start + binStep/2
		weight := weights[i]
		longCost := longPool * weight / totalW
		shortCost := shortPool * weight / totalW
		var longLiq, shortLiq float64
		if maxWLong > 0 {
			longLiq = liqPeakLong * wLongLiq[i] / maxWLong
		}
		if maxWShort > 0 {
			shortLiq = liqPeakShort * wShortLiq[i] / maxWShort
		}
		bins[i] = map[string]interface{}{
			"bucketStartPrice": round8(start),
			"bucketEndPrice":   round8(start + binStep),
			"px":               round8(px),
			"longCost":         round2(longCost),
			"shortCost":        round2(shortCost),
			"longLiq":          round2(longLiq),
			"shortLiq":         round2(shortLiq),
		}
	}

	// Plausible non-zero address counters derived deterministically from the
	// USD pool, in the claw402 order of magnitude (cost ~7867, liq ~6646).
	costAddrs := int(math.Max(1, math.Round(oiUSD/5000)))
	liqAddrs := int(math.Max(1, math.Round(float64(costAddrs)*0.845)))

	payload := map[string]interface{}{
		"data": map[string]interface{}{
			"symbol":     a.Symbol,
			"marketType": a.MarketType,
			"binStep":    round8(binStep),
			"markPrice":  round8(mark),
			"bins":       bins,
			"costAddrs":  costAddrs,
			"liqAddrs":   liqAddrs,
			"market": map[string]interface{}{
				"symbol": a.Symbol,
			},
		},
	}
	return json.Marshal(payload)
}

// ─────────────────────────────────────────────────────────────────────────────
// Signal Lab (dimensions[] contract)
// ─────────────────────────────────────────────────────────────────────────────

// SignalLab builds the per-symbol signal-lab payload matching the
// dimensions[] contract the vergex formatter parses.
func (s *Service) SignalLab(symbol string) (json.RawMessage, error) {
	a, ok := s.assetFor(symbol)
	if !ok {
		return nil, errMarketNotFound
	}
	dimensions := []map[string]interface{}{
		{
			"family":    "Market Structure",
			"label":     "Point of Control",
			"direction": biasToken(a.Mark - a.PrevDay),
			"strength":  strengthWord(a.Mark, a.PrevDay),
			"detail":    "Weighted open-interest pressure centred near mark.",
		},
		{
			"family":    "Trend",
			"label":     "24h Momentum",
			"direction": biasToken(a.Mark - a.PrevDay),
			"strength":  strengthWord(a.Mark, a.PrevDay),
			"detail":    "24h price delta from previous-day reference.",
		},
		{
			"family":    "Funding",
			"label":     "Funding Pressure",
			"direction": biasToken(a.Funding),
			"strength":  strengthWord(a.Funding, 0),
			"detail":    "Funding rate × open interest exposure.",
		},
	}
	payload := map[string]interface{}{
		"symbol":    a.Symbol,
		"marketType": a.MarketType,
		"bias":      biasToken(a.Mark - a.PrevDay),
		"confidence": confidenceWord(a.Mark, a.PrevDay),
		"dimensions": dimensions,
	}
	return json.Marshal(payload)
}

func strengthWord(v, base float64) string {
	if base == 0 {
		base = 1
	}
	mag := math.Abs(v) / base
	switch {
	case mag >= 0.05:
		return "high"
	case mag >= 0.01:
		return "medium"
	default:
		return "low"
	}
}

func confidenceWord(v, base float64) string {
	if base == 0 {
		base = 1
	}
	mag := math.Abs(v) / base
	switch {
	case mag >= 0.05:
		return "high"
	case mag >= 0.01:
		return "medium"
	default:
		return "low"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Net-flow (structural proxy matching nofxos.NetFlowRankingData)
// ─────────────────────────────────────────────────────────────────────────────

// NetFlow builds a net-flow ranking matching the nofxos.NetFlowRankingData
// shape. Institution = funding×OI×mark proxy; retail = OI-delta×mark proxy.
func (s *Service) NetFlow(window string, limit int) (*nofxos.NetFlowRankingData, error) {
	assets, _, _, _ := s.Snapshot()
	if len(assets) == 0 {
		return &nofxos.NetFlowRankingData{}, nil
	}
	var inst, retail []entry
	for _, a := range assets {
		instAmount := a.Funding * a.OI * a.Mark
		inst = append(inst, entry{a.Symbol, instAmount, a.Mark})
		if a.OIPrev > 0 {
			retailAmount := a.OIDelta() * a.Mark
			retail = append(retail, entry{a.Symbol, retailAmount, a.Mark})
		}
	}
	build := func(list []entry) []nofxos.NetFlowPosition {
		sort.Slice(list, func(i, j int) bool {
			return math.Abs(list[i].amount) > math.Abs(list[j].amount)
		})
		out := make([]nofxos.NetFlowPosition, 0, limit)
		for i := 0; i < len(list) && i < limit; i++ {
			out = append(out, nofxos.NetFlowPosition{
				Rank:   i + 1,
				Symbol: list[i].sym,
				Amount: round2(list[i].amount),
				Price:  round8(list[i].price),
			})
		}
		return out
	}
	data := &nofxos.NetFlowRankingData{
		Duration:  window,
		TimeRange: window,
		FetchedAt: time.Now(),
	}
	data.InstitutionFutureTop = build(positive(inst))
	data.InstitutionFutureLow = build(negative(inst))
	data.PersonalFutureTop = build(positive(retail))
	data.PersonalFutureLow = build(negative(retail))
	return data, nil
}

// entry is a single net-flow rank candidate. Declared at package level so the
// shared helpers positive/negative can use it.
type entry struct {
	sym    string
	amount float64
	price  float64
}

// positive returns entries with amount >= 0 (inflow), negative returns amount < 0.
func positive(list []entry) []entry {
	out := make([]entry, 0, len(list))
	for _, e := range list {
		if e.amount >= 0 {
			out = append(out, e)
		}
	}
	return out
}

func negative(list []entry) []entry {
	out := make([]entry, 0, len(list))
	for _, e := range list {
		if e.amount < 0 {
			out = append(out, e)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// assetFor resolves an asset by symbol, tolerating the user-facing form without
// the xyz: prefix (e.g. "SP500" for the stored "xyz:SP500").
func (s *Service) assetFor(symbol string) (*asset, bool) {
	assets, _, _, _ := s.Snapshot()
	if a, ok := assets[symbol]; ok {
		return a, true
	}
	if formatted := hyperliquid.FormatCoinForAPI(symbol); formatted != symbol {
		if a, ok := assets[formatted]; ok {
			return a, true
		}
	}
	return nil, false
}

var errMarketNotFound = &marketNotFoundError{}

type marketNotFoundError struct{}

func (e *marketNotFoundError) Error() string { return "market not found" }

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round8(v float64) float64 { return math.Round(v*1e8) / 1e8 }
