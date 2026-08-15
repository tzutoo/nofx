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
	// Pineify enforces its own rate limit of 40 calls/min (observed 429s
	// beyond that). We default to 40 and never exceed it, and cap the per-ingest
	// budget so a long-tail round-robin cannot blow past the limit.
	rate := s.cfg.PineifyRatePerMinute
	if rate <= 0 || rate > 40 {
		rate = 40
	}
	limiter := newTokenBucket(rate)
	budget := rate // per-ingest call cap (each Pineify call costs one)

	if err := client.initialize(ctx); err != nil {
		s.logger().Warnf("⚠️  Pineify initialize failed, skipping enrichment: %v", err)
		return
	}

	// Mappable items in the prior snapshot's rank order (best-first).
	mappable := s.mappableBoard(assets)
	topK := mappable
	if len(topK) > 10 {
		topK = topK[:10]
	}

	// Priority tier: always enrich top-K, then fill the remaining budget
	// round-robin. Every Pineify call (including failed ones) consumes budget;
	// the remaining budget is threaded in so we never overshoot past it.
	used := 0
	enrich := func(sym string) {
		if used >= budget {
			return
		}
		a := assets[sym]
		snap, calls := s.enrichSymbol(ctx, client, limiter, sym, a.MarketType, budget-used)
		a.Pineify = snap
		s.logger().Infof("  pineify %s -> coverage=%q bias=%q calls=%d", sym, snap.Coverage, snap.Bias, calls)
		used += calls
	}

	for _, sym := range topK {
		enrich(sym)
	}
	if used < budget {
		rest := make([]string, 0, len(mappable))
		for _, sym := range mappable {
			if !contains(topK, sym) {
				rest = append(rest, sym)
			}
		}
		sort.Strings(rest)
		for _, sym := range rest {
			enrich(sym)
		}
	}
	s.logger().Infof("📊 Pineify enrichment complete: %d/%d calls used", used, budget)
}

// mappableBoard returns the xyz: TradeFi (hip3_perp) symbols that map to a
// Pineify ticker, ordered best-first by their composite score so the priority
// tier enriches the board's strongest candidates. Core-perp crypto is excluded
// by default (Pineify crypto augmentation is optional/off-by-default and would
// otherwise consume the whole budget on the long tail).
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

// enrichSymbol fetches the Pineify overlay for one symbol, applying tool
// priority and degrading gracefully on any failure. It returns the snapshot and
// the number of Pineify calls actually made (each limiter.acquire = 1 call).
func (s *Service) enrichSymbol(ctx context.Context, client *pineifyClient, limiter *tokenBucket, sym, marketType string, budgetLeft int) (*PineifySnapshot, int) {
	snap := &PineifySnapshot{FetchedAt: time.Now()}
	base := baseOf(sym)
	ticker := pineifyTicker(base)
	if ticker == "" {
		snap.Error = "not pineify-mappable"
		return snap, 0
	}
	crypto := marketType == "core_perp"
	calls := 0

	// Tier 1: technicals + events. Stop if no budget remains.
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
