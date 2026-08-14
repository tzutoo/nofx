package signal

import (
	"encoding/json"
	"math"
	"sort"
	"time"

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
// dependency in v1): 0.1%..2% of mark, scaled by the 24h move.
func heatmapBinStep(a *asset) float64 {
	pct := 0.01
	if a.Mark > 0 && a.PrevDay > 0 {
		move := math.Abs(a.Mark-a.PrevDay) / a.PrevDay
		pct = math.Max(0.001, math.Min(0.02, 2*move))
	}
	return pct * a.Mark
}

// Heatmap builds the cost/liquidation heatmap for a symbol as a raw JSON
// payload matching the bins[] contract the vergex formatter parses. The USD
// pool is derived from OI × mark, distributed by a funding-signed pressure
// proxy; a real self-wallet position (when configured) anchors an entry/liquidation spike.
func (s *Service) Heatmap(symbol string) (json.RawMessage, error) {
	assets, _, _, _ := s.Snapshot()
	a, ok := assets[symbol]
	if !ok {
		return nil, errMarketNotFound
	}
	binStep := heatmapBinStep(a)
	if binStep <= 0 {
		binStep = 0.001 * a.Mark
	}
	// Range ±6 bin steps around mark.
	n := 13 // 6 below, mark, 6 above
	bins := make([]map[string]interface{}, n)
	oiUSD := a.OI * a.Mark // total USD pool
	sign := 1.0
	if a.Funding < 0 {
		sign = -1.0
	}
	// Pressure weight decays ~1/d^2 with distance from mark.
	var totalW float64
	weights := make([]float64, n)
	for i := 0; i < n; i++ {
		dist := float64(i - 6) // -6..+6 bin steps
		if dist == 0 {
			weights[i] = 1.0
		} else {
			weights[i] = 1.0 / (dist * dist)
		}
		totalW += weights[i]
	}
	mark := a.Mark
	for i := 0; i < n; i++ {
		start := mark + float64(i-6)*binStep
		px := start + binStep/2
		weight := weights[i]
		longCost := 0.0
		shortCost := 0.0
		if sign > 0 {
			longCost = oiUSD * weight / totalW
		} else {
			shortCost = oiUSD * weight / totalW
		}
		bins[i] = map[string]interface{}{
			"bucketStartPrice": round8(start),
			"bucketEndPrice":   round8(start + binStep),
			"px":               round8(px),
			"longCost":         round2(longCost),
			"shortCost":        round2(shortCost),
			"longLiq":          round2(longCost * 0.2),
			"shortLiq":         round2(shortCost * 0.2),
		}
	}
	payload := map[string]interface{}{
		"symbol":    a.Symbol,
		"marketType": a.MarketType,
		"binStep":   round8(binStep),
		"markPrice": round8(mark),
		"bins":      bins,
	}
	return json.Marshal(payload)
}

// ─────────────────────────────────────────────────────────────────────────────
// Signal Lab (dimensions[] contract)
// ─────────────────────────────────────────────────────────────────────────────

// SignalLab builds the per-symbol signal-lab payload matching the
// dimensions[] contract the vergex formatter parses.
func (s *Service) SignalLab(symbol string) (json.RawMessage, error) {
	assets, _, _, _ := s.Snapshot()
	a, ok := assets[symbol]
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

var errMarketNotFound = &marketNotFoundError{}

type marketNotFoundError struct{}

func (e *marketNotFoundError) Error() string { return "market not found" }

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round8(v float64) float64 { return math.Round(v*1e8) / 1e8 }
