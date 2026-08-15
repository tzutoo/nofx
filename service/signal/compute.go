package signal

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
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

	boost := s.cfg != nil && s.cfg.PineifyBoostEnabled
	hardReject := boost && s.cfg != nil && s.cfg.PineifyHardReject
	minConv := 0.7
	if s.cfg != nil && s.cfg.PineifyMinConviction > 0 {
		minConv = s.cfg.PineifyMinConviction
	}

	var demoted []vergex.SignalRankItem
	for rank, sym := range sorted {
		a := assets[sym]
		composite := rawScores[sym]
		conf := 0.0
		if maxAbs > 0 {
			conf = math.Min(1, math.Abs(composite)/maxAbs)
		}
		item := vergex.SignalRankItem{
			Rank:       rank + 1,
			Symbol:     a.Symbol,
			MarketType: a.MarketType,
			Bias:       biasToken(composite),
			Confidence: conf,
			Score:      composite,
			Category:   a.Category,
		}
		// NOTE (F1): we deliberately do NOT write composite onto the shared
		// asset pointer (a.Score). Snapshot() returns live pointers under a read
		// lock that is released on return; mutating them here would race
		// concurrent HTTP handlers. SignalLab recomputes the cohort composite
		// on demand instead.

		// Opt-in Pineify qualifier: when enabled, a mappable item with full
		// coverage whose Pineify bias opposes the HL bias with sufficient
		// conviction is demoted (or excluded with hard-reject).
		if boost && qualifiesForPineifyQualifier(a, composite, minConv) {
			if hardReject {
				continue
			}
			// Halve the magnitude so it ranks below uncontested signals.
			item.Score = composite / 2
			demoted = append(demoted, item)
			continue
		}
		items = append(items, item)
	}
	items = append(items, demoted...)

	return &vergex.SignalRankingData{Items: items}
}

// qualifiesForPineifyQualifier reports whether a mappable asset's full-coverage
// Pineify bias opposes the HL bias strongly enough to demote/reject it. The HL
// composite is passed in (rather than read from a.Score) so this stays a pure,
// read-only decision with no shared-pointer mutation.
func qualifiesForPineifyQualifier(a *asset, composite float64, minConv float64) bool {
	if a == nil || a.Pineify == nil {
		return false
	}
	if a.Pineify.Coverage != "full" || a.Pineify.Bias == "" || composite == 0 {
		return false
	}
	if a.Pineify.Conviction < minConv {
		return false
	}
	hlBias := biasToken(composite)
	pfBias := a.Pineify.Bias
	return (hlBias == "bullish" && pfBias == "bearish") ||
		(hlBias == "bearish" && pfBias == "bullish")
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

// cohortComposite recomputes a per-symbol cohort composite z-score read-only
// against the given cross-section's cohort mean/std. Used by SignalLab to emit
// the compositeZ/score scalars on demand (O(cohort) per request) without
// mutating any shared snapshot pointers (F1).
func (s *Service) cohortComposite(assets map[string]*asset, a *asset) float64 {
	var f [3][]float64 // price24h, funding, oidelta
	for _, other := range assets {
		if other.MarketType != a.MarketType {
			continue
		}
		if d := priceDelta(other); d != nil {
			f[0] = append(f[0], *d)
		}
		f[1] = append(f[1], other.Funding)
		if other.OIPrev > 0 {
			f[2] = append(f[2], other.OIDelta())
		}
	}
	var stats [3]factorStats
	for i := 0; i < 3; i++ {
		stats[i] = meanStd(f[i])
	}
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
	return rankingWeights[0]*z0 + rankingWeights[1]*z1 + rankingWeights[2]*z2
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
	assets, _, _, _ := s.Snapshot()
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
	// Append Pineify overlay rows (rate-limited enrichment, when available).
	// Respects the formatter's 8-row cap: 3 core rows + up to 3 Pineify rows.
	if p := a.Pineify; p != nil && p.Coverage != "" {
		if p.Trend != "" || p.RSI > 0 || p.ADX > 0 {
			dimensions = append(dimensions, map[string]interface{}{
				"family":     "Trend",
				"label":      "Pineify Technical",
				"direction":  emptyDash(p.Bias),
				"strength":   taStrengthWord(p.RSI, p.ADX),
				"percentile": pctNum(p.Conviction),
				"detail":     pineifyTADetail(p),
			})
		}
		if len(p.Events) > 0 {
			dimensions = append(dimensions, map[string]interface{}{
				"family":     "Catalyst",
				"label":      "Upcoming Events",
				"direction":  "",
				"strength":   "medium",
				"percentile": pctNum(p.Conviction),
				"detail":     pineifyEventsDetail(p.Events),
			})
		}
		if p.Rating.Action != "" || p.Rating.Score > 0 {
			dimensions = append(dimensions, map[string]interface{}{
				"family":     "Analyst",
				"label":      "Pineify Rating",
				"direction":  emptyDash(p.Bias),
				"strength":   strengthWord(p.Rating.Score, 100),
				"percentile": pctNum(p.Conviction),
				"detail":     fmt.Sprintf("Pineify overlay: %s (score %.0f)", emptyDash(p.Rating.Action), p.Rating.Score),
			})
		}
	}
	payload := map[string]interface{}{
		"symbol":     a.Symbol,
		"marketType": a.MarketType,
		"bias":       biasToken(a.Mark - a.PrevDay),
		"confidence": confidenceWord(a.Mark, a.PrevDay),
		"dimensions": dimensions,
	}
	// Emit compositeZ/score scalars by recomputing the per-symbol cohort
	// composite on demand (read-only; no reliance on a carried a.Score value).
	if composite := s.cohortComposite(assets, a); composite != 0 {
		payload["compositeZ"] = trimFloat8(composite)
		payload["score"] = trimFloat8(composite)
	}
	// Wrap under `data` with a `meta` envelope to match the frontend
	// VergexSignalLabResponse contract ({data:{...}, meta:{...}}). The heatmap
	// was already data-wrapped; signal-lab was not, so the strategy page read
	// lab?.data as undefined and rendered "Signal Lab has not loaded yet".
	return json.Marshal(map[string]interface{}{
		"data": payload,
		"meta": map[string]interface{}{
			"apiVersion": "v1",
			"chain":      "mainnet",
		},
	})
}

// emptyDash renders "" as "-" for table cells.
func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// taStrengthWord maps RSI/ADX to a strength word for the Pineify technical row.
func taStrengthWord(rsi, adx float64) string {
	if adx >= 25 {
		return "strong"
	}
	if rsi >= 70 || rsi <= 30 {
		return "high"
	}
	if rsi >= 60 || rsi <= 40 {
		return "medium"
	}
	return "low"
}

// pineifyTADetail builds the technical-row detail text. When Source is set
// (crypto TA resolved from a Binance USDT pair), it is appended in parens so
// the AI knows the quote is Binance-derived, not Hyperliquid.
func pineifyTADetail(p *PineifySnapshot) string {
	var parts []string
	if p.Trend != "" {
		parts = append(parts, "trend="+p.Trend)
	}
	if p.RSI > 0 {
		parts = append(parts, fmt.Sprintf("RSI=%.1f", p.RSI))
	}
	if p.ADX > 0 {
		parts = append(parts, fmt.Sprintf("ADX=%.1f", p.ADX))
	}
	suffix := ""
	if p.Source != "" {
		suffix = " (" + p.Source + ")"
	}
	if len(parts) == 0 {
		return "Pineify technical overlay" + suffix + "."
	}
	return "Pineify overlay: " + strings.Join(parts, ", ") + suffix + "."
}

// pineifyEventsDetail summarizes up to 3 upcoming events.
func pineifyEventsDetail(events []PineifyEvent) string {
	if len(events) == 0 {
		return "Pineify overlay: upcoming events."
	}
	var parts []string
	limit := len(events)
	if limit > 3 {
		limit = 3
	}
	for _, e := range events[:limit] {
		name := e.Name
		if name == "" {
			name = e.Type
		}
		if name == "" {
			continue
		}
		if e.Date != "" {
			parts = append(parts, name+" ("+e.Date+")")
		} else {
			parts = append(parts, name)
		}
	}
	if len(parts) == 0 {
		return "Pineify overlay: upcoming events."
	}
	return "Upcoming: " + strings.Join(parts, "; ") + "."
}

// pctNum renders a 0..1 conviction as a 0-100 number for the signal-lab
// percentile bar. The frontend expects a numeric percentile (data.ts:
// percentile?: number), not the old "80%" string, so the bar can render.
func pctNum(v float64) int {
	if v <= 0 {
		return 0
	}
	return int(v * 100)
}

// trimFloat8 renders a float with up to 8 decimals, trimmed.
func trimFloat8(v float64) string {
	s := fmt.Sprintf("%.8f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "-0" {
		return "0"
	}
	return s
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
//
// Fallback order: exact match -> FormatCoinForAPI -> xyz:<bare base>. The last
// fallback makes the snapshot authoritative for xyz assets that are NOT in the
// (possibly stale) hardcoded provider lists (e.g. NBIS, CXMT, SKHY): those are
// stored and served by the proxy as "xyz:NBIS", so a bare "NBIS" that neither
// matches exactly nor via FormatCoinForAPI still resolves by trying the prefixed
// form directly. Crypto bare symbols (e.g. "2Z") match on the exact/first
// fallback and are unaffected.
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
	// Not xyz:-prefixed yet -> try the stored xyz: form of the bare base, using
	// the same base extraction the provider uses. This is authoritative from the
	// snapshot, which knows the true xyz membership (no stale hardcoded list).
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(symbol)), "XYZ:") {
		base := hyperliquid.NormalizeCoinBase(symbol)
		if prefixed := "xyz:" + base; prefixed != symbol {
			if a, ok := assets[prefixed]; ok {
				return a, true
			}
		}
	}
	return nil, false
}

var errMarketNotFound = &marketNotFoundError{}

type marketNotFoundError struct{}

func (e *marketNotFoundError) Error() string { return "market not found" }

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round8(v float64) float64 { return math.Round(v*1e8) / 1e8 }
