package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"nofx/mcp"
	"nofx/provider/hyperliquid"
)

// pineifyEnabled reports whether Pineify augmentation is configured. All
// Pineify calls run inside the single ingest worker (batched per refresh),
// never on the HTTP request path or the trading loop, because Pineify
// rate-limits to a few calls per minute.
func (s *Service) pineifyEnabled() bool {
	return s.cfg != nil && s.cfg.PineifyMCPToken != ""
}

// ─────────────────────────────────────────────────────────────────────────────
// PineifySnapshot — per-symbol enrichment data model
// ─────────────────────────────────────────────────────────────────────────────

// PineifySnapshot holds the per-symbol enrichment overlay fetched from Pineify
// during ingest. Coverage encodes which tool tiers succeeded, so product
// integration can decide whether to render Pineify rows.
type PineifySnapshot struct {
	// Coverage is "" (none) | "ta" | "events" | "rating" | "full".
	Coverage string `json:"coverage"`
	// Bias is the composite Pineify directional bias ("bullish"/"bearish"/
	// "neutral") when derivable, else "".
	Bias string `json:"bias"`
	// Conviction is 0..1 confidence in the Pineify signal.
	Conviction float64 `json:"conviction"`
	// Trend/RSI/ADX/TechScore are technical-overlay values (0 when unavailable).
	Trend     string  `json:"trend"`
	RSI       float64 `json:"rsi"`
	ADX       float64 `json:"adx"`
	TechScore float64 `json:"tech_score"`
	// Events are upcoming catalysts (earnings/news/analyst).
	Events []PineifyEvent `json:"events"`
	// Rating is the analyst action when available.
	Rating PineifyRating `json:"rating"`
	// Error carries any failure text for degraded handling; never a fatal.
	Error string `json:"error"`
	// FetchedAt is when this snapshot was refreshed.
	FetchedAt time.Time `json:"fetched_at"`

	// Source discloses where the data is derived from (e.g. "Binance BTCUSDT"
	// for crypto TA resolved from the Binance USDT pair), so the AI never
	// mistakes a third-party exchange quote for Hyperliquid.
	Source string `json:"source"`
}

// PineifyEvent is a single catalyst/event.
type PineifyEvent struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Date string `json:"date"`
}

// PineifyRating is an analyst action + score.
type PineifyRating struct {
	Action string  `json:"action"`
	Score  float64 `json:"score"`
}

// covered merges the current coverage with an added tier.
func (p *PineifySnapshot) covered(tier string) {
	tiers := []string{"ta", "events", "rating"}
	have := make(map[string]bool)
	for _, part := range strings.Split(p.Coverage, "+") {
		if part != "" {
			have[part] = true
		}
	}
	have[tier] = true
	full := true
	for _, t := range tiers {
		if !have[t] {
			full = false
			break
		}
	}
	if full {
		p.Coverage = "full"
		return
	}
	var parts []string
	for _, t := range tiers {
		if have[t] {
			parts = append(parts, t)
		}
	}
	p.Coverage = strings.Join(parts, "+")
}

// ─────────────────────────────────────────────────────────────────────────────
// Enrichment worker (runs only inside Ingest, gated by token presence)
// ─────────────────────────────────────────────────────────────────────────────

// enrichWithPineify fetches Pineify overlays for the mappable subset of the
// candidate board. Priority: always enrich the top-K mappable items, then
// rotate round-robin through the remaining mappable universe across the
// leftover budget. All calls go through the rate limiter; any failure degrades
// the symbol (never aborts ingest).
func (s *Service) enrichWithPineify(ctx context.Context, assets map[string]*asset) {
	if !s.pineifyEnabled() {
		return
	}
	client := newPineifyClient(s.cfg, s.httpClient, s.logger())
	// Pineify's confirmed hard ceiling is 30 calls/min; we never exceed it and
	// keep headroom for bursts/retries. The per-ingest budget equals the clamped
	// rate (each Pineify call costs one), so a long-tail round-robin cannot blow
	// past the ceiling.
	rate := s.cfg.PineifyRatePerMinute
	if rate <= 0 {
		rate = 1
	}
	if rate > 30 {
		rate = 30
	}
	limiter := newTokenBucket(rate)
	budget := min(rate, 30) // hard cap at the confirmed 30/min ceiling

	if err := client.initialize(ctx); err != nil {
		s.logger().Warnf("⚠️  Pineify initialize failed, skipping enrichment: %v", err)
		return
	}

	// Build the enrichment ordering (see buildEnrichmentOrder):
	//   1. The engine's current candidate set (highest priority — these are the
	//      symbols the AI will actually judge this cycle).
	//   2. A reserved crypto-major floor (cryptoFloorBudget) so crypto is never
	//      starved by the TradeFi top-K, the largest budget consumer.
	//   3. The strongest TradeFi (hip3_perp) mappable items by score (top-K).
	//   4. The remaining crypto majors and TradeFi long tail to fill the budget.
	mappable := s.mappableBoard(assets)
	cryptoMappable := s.cryptoMajorBoard(assets)
	byScore := make([]string, len(mappable))
	copy(byScore, mappable)
	sort.Slice(byScore, func(i, j int) bool { return assets[byScore[i]].Score > assets[byScore[j]].Score })

	priority := s.Priority()
	// Pre-filter engine candidates to the mappable subset (crypto or TradeFi);
	// buildEnrichmentOrder then reserves a crypto floor before the TradeFi top-K
	// so a small budget cannot starve the crypto-major tier.
	var prioMappable []string
	mappableFor := func(a *asset) bool {
		if a == nil {
			return false
		}
		if a.MarketType == "core_perp" {
			return pineifyCryptoTicker(baseOf(a.Symbol)) != ""
		}
		return pineifyTicker(baseOf(a.Symbol)) != ""
	}
	for _, sym := range priority {
		if a := assets[sym]; a != nil && mappableFor(a) {
			prioMappable = append(prioMappable, sym)
		}
	}
	ordered := buildEnrichmentOrder(prioMappable, cryptoMappable, mappable, byScore, budget)
	// consumes budget; the remaining budget is threaded in so we never overshoot.
	used := 0
	for _, sym := range ordered {
		if used >= budget {
			break
		}
		a := assets[sym]
		snap, calls := s.enrichSymbol(ctx, client, limiter, sym, a.MarketType, budget-used)
		a.Pineify = snap
		s.logger().Infof("  pineify %s -> coverage=%q bias=%q calls=%d", sym, snap.Coverage, snap.Bias, calls)
		used += calls
	}
	s.logger().Infof("📊 Pineify enrichment complete: %d/%d calls used", used, budget)
}

// mappableBoard returns the hip3_perp (TradeFi/US-equity) symbols that map to a
// Pineify ticker, ordered best-first by composite score so the strongest
// candidates are enriched first. Core-perp crypto majors are NOT excluded from
// enrichment — they are enumerated separately by cryptoMajorBoard (TA-only) and
// receive a reserved budget floor (see cryptoFloorBudget) so a small budget
// cannot starve them. This function covers only the TradeFi board.
func (s *Service) mappableBoard(assets map[string]*asset) []string {
	var out []string
	for _, a := range assets {
		if a.MarketType != "hip3_perp" {
			continue
		}
		if pineifyTicker(baseOf(a.Symbol)) != "" {
			out = append(out, a.Symbol)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return assets[out[i]].Score > assets[out[j]].Score
	})
	return out
}

// cryptoFloorBudget returns how many crypto-major enrichment calls are reserved
// so the crypto tier is never starved by the TradeFi top-K (the largest budget
// consumer). It is at most 5 and at most a third of the per-ingest budget, so it
// adapts to the budget while keeping TradeFi the higher-value priority.
func cryptoFloorBudget(budget int) int {
	floor := budget / 3
	if floor > 5 {
		floor = 5
	}
	if floor < 0 {
		floor = 0
	}
	return floor
}

// buildEnrichmentOrder assembles the ordered list of symbols to enrich, in the
// order they should consume the per-ingest budget. Engine candidates come
// first (highest priority — what the AI actually judges this cycle). A reserved
// crypto floor is placed next so a small budget cannot starve the crypto-major
// tier, followed by the strongest TradeFi (hip3_perp) by score, then the
// remaining crypto majors, then the TradeFi long tail. callers must pass the
// priority list pre-filtered to mappable symbols.
func buildEnrichmentOrder(priority, cryptoMappable, mappable, byScore []string, budget int) []string {
	floor := cryptoFloorBudget(budget)
	var ordered []string
	seen := make(map[string]bool)
	add := func(sym string) {
		if !seen[sym] {
			ordered = append(ordered, sym)
			seen[sym] = true
		}
	}
	// 1. Engine's current candidate set (highest priority).
	for _, sym := range priority {
		add(sym)
	}
	// 2. Reserved crypto floor: top crypto majors placed before the TradeFi
	//    top-K so a small budget cannot starve the crypto tier.
	reserved := 0
	for _, sym := range cryptoMappable {
		if reserved >= floor {
			break
		}
		if !seen[sym] {
			add(sym)
			reserved++
		}
	}
	// 3. Strongest TradeFi (hip3_perp) by score (top-K).
	tradefiTop := 0
	for _, sym := range byScore {
		if tradefiTop >= 10 {
			break
		}
		if !seen[sym] {
			add(sym)
			tradefiTop++
		}
	}
	// 4. Remaining crypto majors (beyond the floor).
	for _, sym := range cryptoMappable {
		add(sym)
	}
	// 5. Remaining TradeFi (round-robin) to fill the budget.
	for _, sym := range mappable {
		add(sym)
	}
	return ordered
}

// cryptoMajorBoard returns the core_perp crypto-major symbols that map to a
// Pineify ticker (the ~20 Binance USDT majors that also trade on Hyperliquid),
// ordered best-first by composite score. Crypto majors are enriched with TA
// ONLY — events and rating are US-equity centric and Pineify returns nothing
// for them, so they never consume a budget slot.
func (s *Service) cryptoMajorBoard(assets map[string]*asset) []string {
	var out []string
	for _, a := range assets {
		if a.MarketType != "core_perp" {
			continue
		}
		if pineifyCryptoTicker(baseOf(a.Symbol)) != "" {
			out = append(out, a.Symbol)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return assets[out[i]].Score > assets[out[j]].Score
	})
	return out
}

// enrichSymbol fetches the Pineify overlay for one symbol, applying tool
// priority and degrading gracefully on any failure. It returns the snapshot and
// the number of Pineify calls actually made (each limiter.acquire = 1 call).
func (s *Service) enrichSymbol(ctx context.Context, client *pineifyClient, limiter *tokenBucket, sym, marketType string, budgetLeft int) (*PineifySnapshot, int) {
	snap := &PineifySnapshot{FetchedAt: time.Now()}
	base := baseOf(sym)
	crypto := marketType == "core_perp"
	var ticker string
	if crypto {
		ticker = pineifyCryptoTicker(base)
	} else {
		ticker = pineifyTicker(base)
	}
	if ticker == "" {
		snap.Error = "not pineify-mappable"
		return snap, 0
	}
	// Source disclosure: crypto TA is resolved from the Binance USDT pair
	// (<BASE>USDT), not from Hyperliquid — surface that so the AI never
	// mistakes a third-party exchange quote for HL.
	if crypto {
		snap.Source = "Binance " + cryptoTicker(ticker, true)
	}
	calls := 0

	// Tier 1: technicals. Events are skipped for crypto (get-stock-event-context
	// is US-equity centric; Pineify returns nothing for crypto). Stop if no
	// budget remains.
	if budgetLeft <= 0 {
		snap.Error = "budget exhausted"
		return snap, calls
	}
	if err := limiter.acquire(ctx); err != nil {
		snap.Error = err.Error()
		return snap, calls
	}
	calls++
	ta, err := client.callTool(ctx, "get-technical-analysis-snapshot", map[string]any{
		"symbol":     cryptoTicker(ticker, crypto),
		"timeframes": []string{"1d"},
	})
	if err != nil {
		s.logger().Warnf("⚠️  Pineify TA failed for %s: %v", sym, err)
	} else if snap.applyTA(ta) {
		snap.covered("ta")
	}

	// Events (US stocks only; Pineify event-context is US-equity centric).
	if !crypto {
		if budgetLeft <= calls {
			snap.Error = "budget exhausted"
			return snap, calls
		}
		if err := limiter.acquire(ctx); err != nil {
			snap.Error = err.Error()
			return snap, calls
		}
		calls++
		events, err := client.callTool(ctx, "get-stock-event-context", map[string]any{
			"symbol": ticker,
		})
		if err != nil {
			s.logger().Warnf("⚠️  Pineify events failed for %s: %v", sym, err)
		} else if snap.applyEvents(events) {
			snap.covered("events")
		}
	}

	// Tier 2: rating (US stocks only).
	if !crypto && budgetLeft > calls {
		if err := limiter.acquire(ctx); err != nil {
			snap.Error = err.Error()
			return snap, calls
		}
		calls++
		rating, err := client.callTool(ctx, "get-ai-stock-rating", map[string]any{
			"symbol": ticker,
		})
		if err != nil {
			s.logger().Warnf("⚠️  Pineify rating failed for %s: %v", sym, err)
		} else if snap.applyRating(rating) {
			snap.covered("rating")
		}
	}

	snap.computeBias()
	return snap, calls
}

// applyTA parses the technical-analysis-snapshot structuredContent
// (`snapshots[0].values`) and fills Trend/RSI/ADX.
func (p *PineifySnapshot) applyTA(raw json.RawMessage) bool {
	ta, ok := parsePineifyTA(raw)
	if !ok {
		return false
	}
	p.Trend = ta.Trend
	p.RSI = ta.RSI
	p.ADX = ta.ADX
	p.TechScore = ta.Score
	return p.Trend != "" || p.RSI > 0 || p.ADX > 0 || p.TechScore > 0
}

// applyEvents parses upcoming/recent events from structuredContent.data.
func (p *PineifySnapshot) applyEvents(raw json.RawMessage) bool {
	evs := parsePineifyEvents(raw)
	p.Events = evs
	return len(evs) > 0
}

// applyRating parses the analyst rating from structuredContent.rating.
func (p *PineifySnapshot) applyRating(raw json.RawMessage) bool {
	r, ok := parsePineifyRating(raw)
	if !ok {
		return false
	}
	p.Rating = r
	return r.Action != "" || r.Score > 0
}

// computeBias derives the composite directional bias from the overlay.
func (p *PineifySnapshot) computeBias() {
	if p.Trend != "" {
		low := strings.ToLower(p.Trend)
		switch {
		case strings.Contains(low, "bull") || strings.Contains(low, "up") || strings.Contains(low, "strong buy"):
			p.Bias = "bullish"
			p.Conviction = 0.8
			return
		case strings.Contains(low, "bear") || strings.Contains(low, "down") || strings.Contains(low, "strong sell"):
			p.Bias = "bearish"
			p.Conviction = 0.8
			return
		}
	}
	if p.Rating.Action != "" {
		switch strings.ToLower(p.Rating.Action) {
		case "buy", "strong buy", "outperform":
			p.Bias = "bullish"
			p.Conviction = 0.7
			return
		case "sell", "strong sell", "underperform":
			p.Bias = "bearish"
			p.Conviction = 0.7
			return
		}
	}
}

// baseOf strips the xyz:/USDT/etc. prefix to the bare base symbol.
func baseOf(symbol string) string {
	return hyperliquid.NormalizeCoinBase(symbol)
}

// cryptoTicker returns the bare Pineify ticker for a crypto symbol (e.g. BTC →
// BTCUSDT) or the stock ticker unchanged.
func cryptoTicker(ticker string, crypto bool) string {
	if crypto && !strings.HasSuffix(strings.ToUpper(ticker), "USDT") {
		return strings.ToUpper(ticker) + "USDT"
	}
	return strings.ToUpper(ticker)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// logger returns the service's mcp.Logger (non-nil).
func (s *Service) logger() mcp.Logger {
	if s.log != nil {
		return s.log
	}
	return mcp.NewNoopLogger()
}

// ─────────────────────────────────────────────────────────────────────────────
// Pineify response parsing (defensive, field-order/case-insensitive)
// ─────────────────────────────────────────────────────────────────────────────

type pineifyTA struct {
	Trend string
	RSI   float64
	ADX   float64
	Score float64
}

// parsePineifyTA reads the technical-analysis-snapshot structuredContent.
// Shape: { snapshots: [{ timeframe, values:{ signal, score, trend_score, rsi14, adx14, ... } }] }
func parsePineifyTA(raw json.RawMessage) (pineifyTA, bool) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return pineifyTA{}, false
	}
	var values map[string]any
	if snaps, ok := doc["snapshots"].([]any); ok && len(snaps) > 0 {
		if first, ok := snaps[0].(map[string]any); ok {
			if v, ok := first["values"].(map[string]any); ok {
				values = v
			}
		}
	}
	if values == nil {
		return pineifyTA{}, false
	}
	ta := pineifyTA{}
	if signal, ok := lookupAny(values, "signal", "trend", "bias", "direction"); ok {
		ta.Trend = asString(signal)
	}
	if score, ok := lookupAny(values, "score", "composite_technical_score", "compositeScore"); ok {
		ta.Score = asFloat(score)
	}
	if rsi, ok := lookupAny(values, "rsi14", "rsi_14", "rsi"); ok {
		ta.RSI = asFloat(rsi)
	}
	if adx, ok := lookupAny(values, "adx14", "adx_14", "adx"); ok {
		ta.ADX = asFloat(adx)
	}
	return ta, ta.Trend != "" || ta.RSI > 0 || ta.ADX > 0 || ta.Score > 0
}

// parsePineifyEvents reads the event-context structuredContent.
// Shape: { data: { upcomingEvents:[{eventType,date,...}], recentEvents:[{eventType,title,...}] } }
func parsePineifyEvents(raw json.RawMessage) []PineifyEvent {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var data map[string]any
	if d, ok := doc["data"].(map[string]any); ok {
		data = d
	} else {
		data = doc
	}
	var out []PineifyEvent
	if up, ok := data["upcomingEvents"].([]any); ok {
		for _, row := range up {
			m, ok := row.(map[string]any)
			if !ok {
				continue
			}
			e := PineifyEvent{Type: firstStr(m, "eventType", "event_type", "type")}
			if e.Date == "" {
				e.Date = firstStr(m, "date", "eventAt", "event_at", "when")
			}
			if e.Name == "" {
				e.Name = firstStr(m, "title", "name", "label")
			}
			if e.Name == "" {
				e.Name = e.Type
			}
			out = append(out, e)
		}
	}
	if len(out) < 3 {
		if recent, ok := data["recentEvents"].([]any); ok {
			for _, row := range recent {
				if len(out) >= 3 {
					break
				}
				m, ok := row.(map[string]any)
				if !ok {
					continue
				}
				e := PineifyEvent{Type: firstStr(m, "eventType", "event_type", "type")}
				if e.Name == "" {
					e.Name = firstStr(m, "title", "name")
				}
				if e.Date == "" {
					e.Date = firstStr(m, "eventAt", "event_at", "date")
				}
				if e.Name == "" {
					continue
				}
				out = append(out, e)
			}
		}
	}
	return out
}

// parsePineifyRating reads the ai-stock-rating structuredContent.
// Shape: { instrument:{symbol,...}, rating:{ overallRank, scores:{overall,...} } }
func parsePineifyRating(raw json.RawMessage) (PineifyRating, bool) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return PineifyRating{}, false
	}
	rating, ok := doc["rating"].(map[string]any)
	if !ok {
		rating = doc
	}
	r := PineifyRating{}
	if scores, ok := rating["scores"].(map[string]any); ok {
		if overall, ok := lookupAny(scores, "overall", "total", "composite"); ok {
			r.Score = asFloat(overall)
		}
	}
	if r.Score <= 0 {
		if score, ok := lookupAny(rating, "overallScore", "score"); ok {
			r.Score = asFloat(score)
		}
	}
	// Map the technical score to an action when a clear threshold exists.
	if r.Score >= 7 {
		r.Action = "buy"
	} else if r.Score <= 4 {
		r.Action = "sell"
	} else {
		r.Action = "neutral"
	}
	return r, r.Action != "" || r.Score > 0
}

// lookupAny finds the first present key (case/separator-insensitive).
func lookupAny(obj map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if v, ok := lookupVal(obj, key); ok {
			return v, true
		}
	}
	return nil, false
}

func lookupVal(obj map[string]any, keys ...string) (any, bool) {
	want := normalizeKeyAny(keys[0])
	for _, k := range keys {
		_ = k
		for kk, v := range obj {
			if normalizeKeyAny(kk) == want {
				return v, true
			}
		}
	}
	return nil, false
}

func normalizeKeyAny(key string) string {
	replacer := strings.NewReplacer("_", "", "-", "", " ", "", ".", "")
	return replacer.Replace(strings.ToLower(strings.TrimSpace(key)))
}

func firstStr(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := lookupVal(obj, k); ok {
			if s := asString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		var f float64
		if _, err := fmt.Sscanf(t, "%f", &f); err == nil {
			return f
		}
	}
	return 0
}
