package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// pineifyTicker mapping
// ---------------------------------------------------------------------------

func TestPineifyTickerMapping(t *testing.T) {
	cases := map[string]string{
		"xyz:NVDA":  "NVDA",
		"xyz:MU":    "MU",
		"xyz:SNDK":  "SNDK",
		"xyz:NBIS":  "NBIS",
		"NVDA":      "NVDA",
		"xyz:SP500": "",    // index
		"xyz:JP225": "",    // index
		"xyz:KR200": "",    // index
		"xyz:DXY":   "",    // index
		"xyz:GOLD":  "",    // commodity
		"xyz:SMSN":  "",    // non-US exclusion
		"xyz:SKHX":  "",    // non-US exclusion
		"xyz:BTC":   "BTC", // crypto major maps to base; enriched as BTCUSDT
		"":          "",
	}
	for in, want := range cases {
		got := pineifyTicker(baseOf(in))
		if got != want {
			t.Errorf("pineifyTicker(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// pineifyCryptoTicker mapping (crypto-major path)
// ---------------------------------------------------------------------------

func TestPineifyCryptoTickerMapping(t *testing.T) {
	majors := map[string]string{
		"BTC": "BTC",
		"ETH": "ETH",
		"SOL": "SOL",
		"XRP": "XRP",
		"SUI": "SUI",
		"":    "",
	}
	for in, want := range majors {
		got := pineifyCryptoTicker(baseOf(in))
		if got != want {
			t.Errorf("pineifyCryptoTicker(%q) = %q, want %q", in, got, want)
		}
	}
	// Long-tail crypto and non-crypto must NOT map via the crypto path.
	nonMajors := []string{"XYZ:SOMELONGTAIL", "XYZ:PEPE123", "NVDA", "SP500"}
	for _, in := range nonMajors {
		if got := pineifyCryptoTicker(baseOf(in)); got != "" {
			t.Errorf("pineifyCryptoTicker(%q) = %q, want \"\" (not a crypto major)", in, got)
		}
	}
}

// TestEnrichCryptoMajorTAOnly verifies a crypto major is enriched with TA only
// (events + rating are skipped) and the source is disclosed as Binance.
func TestEnrichCryptoMajorTAOnly(t *testing.T) {
	srv := newFakeMCP()
	ts := newTestHTTPServer(srv.handle())
	defer ts.Close()

	cfg := &Config{
		PineifyMCPToken:      "test-token",
		PineifyBaseURL:       ts.URL,
		PineifyRatePerMinute: 100,
	}
	client := newPineifyClient(cfg, nil, nil)
	ctx := context.Background()
	if err := client.initialize(ctx); err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
	s := &Service{cfg: cfg, httpClient: ts.Client()}
	limiter := newTokenBucket(100)
	snap, calls := s.enrichSymbol(ctx, client, limiter, "BTC", "core_perp", 10, time.Time{})
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if snap.Coverage != "ta" {
		t.Errorf("coverage = %q, want ta (events/rating skipped for crypto)", snap.Coverage)
	}
	if snap.RSI != 62 {
		t.Errorf("rsi = %v, want 62", snap.RSI)
	}
	if snap.Source != "Binance BTCUSDT" {
		t.Errorf("source = %q, want Binance BTCUSDT (Binance disclosure)", snap.Source)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (TA only)", calls)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.toolCalls["get-technical-analysis-snapshot"] != 1 {
		t.Errorf("TA called %d times, want 1", srv.toolCalls["get-technical-analysis-snapshot"])
	}
	if srv.toolCalls["get-stock-event-context"] != 0 {
		t.Errorf("events should NOT be called for crypto, got %d", srv.toolCalls["get-stock-event-context"])
	}
	if srv.toolCalls["get-ai-stock-rating"] != 0 {
		t.Errorf("rating should NOT be called for crypto, got %d", srv.toolCalls["get-ai-stock-rating"])
	}
}

// TestEnrichCryptoNonMajorNotMappable verifies a long-tail crypto base (not in
// the crypto-major set) is not mappable via the crypto path.
func TestEnrichCryptoNonMajorNotMappable(t *testing.T) {
	cfg := &Config{PineifyMCPToken: "x", PineifyRatePerMinute: 10}
	s := &Service{cfg: cfg}
	client := newPineifyClient(cfg, nil, nil)
	limiter := newTokenBucket(10)
	snap, calls := s.enrichSymbol(context.Background(), client, limiter, "SOMELONGTAIL", "core_perp", 10, time.Time{})
	if snap == nil || snap.Error == "" {
		t.Fatalf("expected error for non-major crypto, got %+v", snap)
	}
	if snap.Coverage != "" {
		t.Errorf("coverage should be empty for non-major crypto, got %q", snap.Coverage)
	}
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (not mappable)", calls)
	}
}

// ---------------------------------------------------------------------------
// Rate limiter
// ---------------------------------------------------------------------------

func TestTokenBucketPacesCalls(t *testing.T) {
	b := newTokenBucket(60) // 60/min -> 1s spacing; starts empty (no pre-fill)
	ctx := context.Background()
	start := time.Now()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	// Back-to-back acquires must be spaced, never fired instantly — that burst
	// is what tripped Pineify's wall-clock limit.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("back-to-back acquires should be paced ~1s apart (rate=60), took %v", elapsed)
	}
}

// TestTokenBucketFullDoesNotBurst verifies that even a bucket which has idled
// back to full capacity still spaces calls, so a later ingest cannot burst past
// the per-minute rate either.
func TestTokenBucketFullDoesNotBurst(t *testing.T) {
	b := newTokenBucket(60) // 1s spacing
	b.mu.Lock()
	b.tokens = b.capacity // simulate a fully-refilled (idled) bucket
	b.mu.Unlock()
	ctx := context.Background()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("first acquire errored: %v", err)
	}
	// The second grant must still wait for minSpacing even though tokens remain.
	start := time.Now()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("second acquire errored: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("full-capacity bucket should still space calls ~1s, took %v", elapsed)
	}
}

func TestTokenBucketCancellation(t *testing.T) {
	b := newTokenBucket(60) // 1s spacing
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Empty (un-pre-filled) bucket blocks; a short ctx must abort the acquire.
	if err := b.acquire(ctx); err != context.DeadlineExceeded {
		t.Fatalf("acquire with no token and short ctx should be cancelled, got %v", err)
	}
}

func TestCryptoFloorBudget(t *testing.T) {
	cases := map[int]int{
		1:   0,
		2:   0,
		3:   1,
		5:   1,
		6:   2,
		9:   3,
		15:  5,
		20:  5,
		30:  5,
		100: 5,
	}
	for budget, want := range cases {
		if got := cryptoFloorBudget(budget); got != want {
			t.Errorf("cryptoFloorBudget(%d) = %d, want %d", budget, got, want)
		}
	}
}

// TestCryptoFloorReservedInOrder verifies the crypto floor is placed ahead of
// the TradeFi top-K, so a small budget (which the TradeFi top-K alone would
// exhaust) cannot starve the crypto-major tier.
func TestCryptoFloorReservedInOrder(t *testing.T) {
	crypto := []string{"BTC", "ETH", "SOL", "XRP"}
	mappable := []string{"xyz:NVDA", "xyz:AAPL", "xyz:MSFT"}
	byScore := []string{"xyz:NVDA", "xyz:AAPL", "xyz:MSFT"}

	// Small budget 6 -> floor = min(5, 6/3) = 2 crypto majors reserved.
	ordered := buildEnrichmentOrder(nil, crypto, mappable, byScore, 6)
	pos := func(s string) int {
		for i, x := range ordered {
			if x == s {
				return i
			}
		}
		return -1
	}
	if pos("BTC") < 0 || pos("ETH") < 0 {
		t.Fatalf("crypto floor majors missing from order: %v", ordered)
	}
	// The floor must come ahead of the TradeFi top-K so the budget reaches it.
	if pos("BTC") > pos("xyz:NVDA") {
		t.Errorf("crypto floor (BTC) should be ordered before TradeFi top-K, got %v", ordered)
	}
	if pos("ETH") > pos("xyz:NVDA") {
		t.Errorf("crypto floor (ETH) should be ordered before TradeFi top-K, got %v", ordered)
	}
}

// ---------------------------------------------------------------------------
// Enrichment / degraded handling via httptest MCP server
// ---------------------------------------------------------------------------

// fakeMCP is a minimal fake MCP JSON-RPC server for tests. It returns canned
// tool results and records calls.
type fakeMCP struct {
	mu        sync.Mutex
	calls     map[string]int
	toolCalls map[string]int
	taJSON    string
	events    string
	rating    string
	// delay, when > 0, makes the fake Pineify server sleep before each reply so
	// tests can exercise the wall-clock enrichment deadline (F8).
	delay time.Duration
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{
		calls:     make(map[string]int),
		toolCalls: make(map[string]int),
		taJSON:    `{"snapshots":[{"timeframe":"1d","values":{"signal":"bullish","score":82,"rsi14":62,"adx14":28}}]}`,
		events:    `{"data":{"upcomingEvents":[{"eventType":"earnings","date":"2026-08-26","epsEstimate":2.08}],"recentEvents":[{"eventType":"news","title":"NVDA news","eventAt":"2026-08-14T20:02:10.000Z"}]}}`,
		rating:    `{"instrument":{"symbol":"NVDA"},"rating":{"overallRank":295,"scores":{"overall":8,"change":0,"fundamental":9,"technical":3}}}`,
	}
}

func (f *fakeMCP) handle() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			ID     int64          `json:"id"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls[req.Method]++
		f.mu.Unlock()

		if f.delay > 0 {
			time.Sleep(f.delay)
		}

		switch req.Method {
		case "initialize":
			writeMCPResult(w, req.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "pineify-test"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/call":
			name, _ := req.Params["name"].(string)
			f.mu.Lock()
			f.toolCalls[name]++
			f.mu.Unlock()
			var result string
			switch name {
			case "get-technical-analysis-snapshot":
				result = f.taJSON
			case "get-stock-event-context":
				result = f.events
			case "get-ai-stock-rating":
				result = f.rating
			}
			var structured json.RawMessage
			_ = json.Unmarshal([]byte(result), &structured)
			writeMCPResult(w, req.ID, map[string]any{
				"content":           []map[string]any{{"type": "text", "text": "summary " + name}},
				"structuredContent": structured,
			})
		default:
			writeMCPError(w, req.ID, -32601, "method not found")
		}
	}
}

func writeMCPResult(w http.ResponseWriter, id int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeMCPError(w http.ResponseWriter, id int64, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}

func TestEnrichSymbolWithMCP(t *testing.T) {
	srv := newFakeMCP()
	ts := newTestHTTPServer(srv.handle())
	defer ts.Close()

	cfg := &Config{
		PineifyMCPToken:      "test-token",
		PineifyBaseURL:       ts.URL,
		PineifyRatePerMinute: 100,
	}
	client := newPineifyClient(cfg, nil, nil)
	ctx := context.Background()
	if err := client.initialize(ctx); err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
	s := &Service{cfg: cfg, httpClient: ts.Client()}
	limiter := newTokenBucket(100)
	snap, _ := s.enrichSymbol(ctx, client, limiter, "xyz:NVDA", "hip3_perp", 10, time.Time{})
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if snap.Coverage != "full" {
		t.Errorf("coverage = %q, want full", snap.Coverage)
	}
	if snap.Bias != "bullish" {
		t.Errorf("bias = %q, want bullish", snap.Bias)
	}
	if snap.RSI != 62 {
		t.Errorf("rsi = %v, want 62", snap.RSI)
	}
	if len(snap.Events) < 1 {
		t.Errorf("events = %d, want >=1 (upcoming + recent)", len(snap.Events))
	}
	if snap.Rating.Action != "buy" {
		t.Errorf("rating action = %q, want buy", snap.Rating.Action)
	}
}

// TestEnrichWithPineifyCryptoCoverage runs the full enrichment pipeline with a
// mixed TradeFi + crypto board and verifies crypto majors are actually enriched
// (not starved) while TradeFi still gets full coverage.
func TestEnrichWithPineifyCryptoCoverage(t *testing.T) {
	srv := newFakeMCP()
	ts := newTestHTTPServer(srv.handle())
	defer ts.Close()
	cfg := &Config{
		PineifyMCPToken:      "test-token",
		PineifyBaseURL:       ts.URL,
		PineifyRatePerMinute: 30, // budget 30, crypto floor 5; min spacing 2s
	}
	s := &Service{cfg: cfg, httpClient: ts.Client()}
	assets := map[string]*asset{
		"xyz:NVDA": {Symbol: "xyz:NVDA", MarketType: "hip3_perp", Score: 90},
		"BTC":      {Symbol: "BTC", MarketType: "core_perp", Score: 80},
		"ETH":      {Symbol: "ETH", MarketType: "core_perp", Score: 70},
	}
	s.enrichWithPineify(context.Background(), assets)
	if got := coverageOf(assets["BTC"]); got != "ta" {
		t.Errorf("BTC pineify coverage = %q, want ta (crypto not starved)", got)
	}
	if got := coverageOf(assets["ETH"]); got != "ta" {
		t.Errorf("ETH pineify coverage = %q, want ta (crypto not starved)", got)
	}
	if got := coverageOf(assets["xyz:NVDA"]); got != "full" {
		t.Errorf("NVDA pineify coverage = %q, want full (TradeFi still enriched)", got)
	}
}

func coverageOf(a *asset) string {
	if a == nil || a.Pineify == nil {
		return ""
	}
	return a.Pineify.Coverage
}

func TestEnrichSymbolNotMappable(t *testing.T) {
	cfg := &Config{PineifyMCPToken: "x", PineifyRatePerMinute: 10}
	s := &Service{cfg: cfg}
	client := newPineifyClient(cfg, nil, nil)
	limiter := newTokenBucket(10)
	snap, _ := s.enrichSymbol(context.Background(), client, limiter, "xyz:SP500", "hip3_perp", 10, time.Time{})
	if snap == nil || snap.Error == "" {
		t.Fatalf("expected error for non-mappable symbol, got %+v", snap)
	}
	if snap.Coverage != "" {
		t.Errorf("coverage should be empty for non-mappable, got %q", snap.Coverage)
	}
}

func TestEnrichSymbolDegraded(t *testing.T) {
	cfg := &Config{PineifyMCPToken: "x", PineifyRatePerMinute: 10}
	s := &Service{cfg: cfg}
	client := newPineifyClient(cfg, nil, nil)
	limiter := newTokenBucket(0) // no budget → immediate failure
	// limiter rate 0 is clamped to 1 internally; use a pre-exhausted bucket via
	// cancelled context instead.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap, _ := s.enrichSymbol(ctx, client, limiter, "xyz:NVDA", "hip3_perp", 10, time.Time{})
	if snap == nil || snap.Coverage != "" {
		t.Fatalf("expected degraded (no coverage) on cancellation, got %+v", snap)
	}
	if snap.Error == "" {
		t.Fatal("expected error on cancellation")
	}
}

// ---------------------------------------------------------------------------
// Signal Lab enrichment integration
// ---------------------------------------------------------------------------

func TestSignalLabWithPineifyRows(t *testing.T) {
	s := testService([]*asset{
		{Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 100, PrevDay: 95, Funding: 0.001},
	})
	s.mu.Lock()
	s.assets["xyz:NVDA"].Pineify = &PineifySnapshot{
		Coverage:   "full",
		Bias:       "bullish",
		Conviction: 0.8,
		Trend:      "bullish",
		RSI:        62,
		ADX:        28,
		Events:     []PineifyEvent{{Type: "earnings", Name: "Q3", Date: "2026-09-01"}},
		Rating:     PineifyRating{Action: "buy", Score: 80},
	}
	s.mu.Unlock()

	body, err := s.SignalLab("xyz:NVDA")
	if err != nil {
		t.Fatalf("SignalLab errored: %v", err)
	}
	var out struct {
		Data struct {
			Dimensions []map[string]any `json:"dimensions"`
			CompositeZ string           `json:"compositeZ"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Data.Dimensions) != 6 {
		t.Fatalf("dimensions = %d, want 6 (3 core + 3 pineify)", len(out.Data.Dimensions))
	}
	// Last row should be the Analyst/Pineify Rating row with a numeric
	// percentile (0-100), not a "80%" string.
	last := out.Data.Dimensions[len(out.Data.Dimensions)-1]
	if last["family"] != "Analyst" {
		t.Errorf("last family = %v, want Analyst", last["family"])
	}
	pctVal, ok := last["percentile"].(float64)
	if !ok {
		t.Errorf("pineify row percentile should be a number, got %T %v", last["percentile"], last["percentile"])
	} else if pctVal < 1 || pctVal > 100 {
		t.Errorf("pineify row percentile = %v, want within 1..100", pctVal)
	}
}

// ---------------------------------------------------------------------------
// Opt-in ranking qualifier
// ---------------------------------------------------------------------------

func TestRankPineifyQualifierDefaultOff(t *testing.T) {
	cfg := &Config{PineifyBoostEnabled: false, PineifyMinConviction: 0.7}
	s := testService([]*asset{
		{Symbol: "AAA", MarketType: "core_perp", Category: "crypto", Mark: 100, PrevDay: 90, Funding: 0.001, OI: 1000, OIPrev: 900},
		{Symbol: "BBB", MarketType: "core_perp", Category: "crypto", Mark: 50, PrevDay: 60, Funding: -0.001, OI: 500, OIPrev: 450},
	})
	s.cfg = cfg
	// BBB has a pineify overlay opposing (HL bearish, pineify bullish).
	s.mu.Lock()
	s.assets["BBB"].Pineify = &PineifySnapshot{Coverage: "full", Bias: "bullish", Conviction: 0.9}
	s.mu.Unlock()

	board := s.Rank(10)
	if len(board.Items) != 2 {
		t.Fatalf("items = %d, want 2 (boost off keeps both)", len(board.Items))
	}
}

func TestRankPineifyQualifierHardReject(t *testing.T) {
	cfg := &Config{PineifyBoostEnabled: true, PineifyHardReject: true, PineifyMinConviction: 0.7}
	s := testService([]*asset{
		{Symbol: "AAA", MarketType: "core_perp", Category: "crypto", Mark: 100, PrevDay: 90, Funding: 0.001, OI: 1000, OIPrev: 900},
		{Symbol: "BBB", MarketType: "core_perp", Category: "crypto", Mark: 50, PrevDay: 60, Funding: -0.001, OI: 500, OIPrev: 450},
	})
	s.cfg = cfg
	// BBB's HL composite is bearish (Mark<PrevDay) with a pineify overlay
	// opposing (pineify bullish), so with boost+hardReject it is excluded.
	s.mu.Lock()
	s.assets["BBB"].Pineify = &PineifySnapshot{Coverage: "full", Bias: "bullish", Conviction: 0.9}
	s.mu.Unlock()

	board := s.Rank(10)
	for _, it := range board.Items {
		if it.Symbol == "BBB" {
			t.Fatalf("BBB should be hard-rejected, but present in board")
		}
	}
	if len(board.Items) != 1 {
		t.Errorf("items = %d, want 1 (BBB excluded)", len(board.Items))
	}
}

func newTestHTTPServer(h http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(h)
}

// ---------------------------------------------------------------------------
// Source disclosure in the Signal Lab technical detail
// ---------------------------------------------------------------------------

func TestPineifyTADetailSourceDisclosure(t *testing.T) {
	// Crypto TA carries a Binance source so the AI knows it's not HL-derived.
	got := pineifyTADetail(&PineifySnapshot{Trend: "bullish", RSI: 62, ADX: 28, Source: "Binance BTCUSDT"})
	if !strings.Contains(got, "Binance BTCUSDT") {
		t.Errorf("detail = %q, want it to disclose Binance source", got)
	}
	// No source -> no parens appended.
	got2 := pineifyTADetail(&PineifySnapshot{Trend: "bullish", RSI: 62})
	if strings.Contains(got2, "Binance") {
		t.Errorf("detail = %q, want no Binance source for non-crypto", got2)
	}
}

// ---------------------------------------------------------------------------
// Snapshot persistence across ingests
// ---------------------------------------------------------------------------

func TestPineifySnapshotPersistence(t *testing.T) {
	s := testService([]*asset{{Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 100, PrevDay: 95}})
	// Simulate a prior snapshot with a Pineify overlay fetched recently.
	s.mu.Lock()
	s.assets["xyz:NVDA"].Pineify = &PineifySnapshot{Coverage: "full", Bias: "bullish", Conviction: 0.8, FetchedAt: time.Now().Add(-time.Minute)}
	s.mu.Unlock()

	// New ingest with a fresh asset (no Pineify yet) for the same symbol.
	assets := map[string]*asset{"xyz:NVDA": {Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 101, PrevDay: 95}}
	s.mu.Lock()
	s.carryForward(s.assets, assets)
	s.assets = assets
	s.mu.Unlock()

	// Carry-forward should restore the previous Pineify snapshot onto the new asset.
	if assets["xyz:NVDA"].Pineify == nil || assets["xyz:NVDA"].Pineify.Coverage != "full" {
		t.Fatalf("Pineify snapshot not persisted across ingest: %+v", assets["xyz:NVDA"].Pineify)
	}
}

// ---------------------------------------------------------------------------
// F4: stale carried Pineify snapshots are age-out so they aren't rendered
// ---------------------------------------------------------------------------

func TestPineifyStaleAgeOut(t *testing.T) {
	s := testService([]*asset{{Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 100, PrevDay: 95}})
	s.cfg.Interval = 3 * time.Minute // 6×3m = 18m staleness window
	// Overlay fetched well beyond the window -> must be aged out.
	s.mu.Lock()
	s.assets["xyz:NVDA"].Pineify = &PineifySnapshot{Coverage: "full", Bias: "bullish", Conviction: 0.8, FetchedAt: time.Now().Add(-40 * time.Minute)}
	s.mu.Unlock()

	assets := map[string]*asset{"xyz:NVDA": {Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 101, PrevDay: 95}}
	s.mu.Lock()
	s.carryForward(s.assets, assets)
	s.mu.Unlock()

	p := assets["xyz:NVDA"].Pineify
	if p == nil {
		t.Fatal("expected carried snapshot to be marked stale (not nil)")
	}
	if p.Coverage != "" {
		t.Errorf("stale snapshot Coverage = %q, want cleared so it's not rendered", p.Coverage)
	}
	if p.Error != "stale" {
		t.Errorf("stale snapshot Error = %q, want \"stale\"", p.Error)
	}
}

func TestPineifyWithinWindowCarriedFresh(t *testing.T) {
	s := testService([]*asset{{Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 100, PrevDay: 95}})
	s.cfg.Interval = 3 * time.Minute
	// Overlay fetched 2 minutes ago -> within the 18m window -> carried intact.
	s.mu.Lock()
	s.assets["xyz:NVDA"].Pineify = &PineifySnapshot{Coverage: "full", Bias: "bullish", Conviction: 0.8, FetchedAt: time.Now().Add(-2 * time.Minute)}
	s.mu.Unlock()

	assets := map[string]*asset{"xyz:NVDA": {Symbol: "xyz:NVDA", MarketType: "hip3_perp", Category: "stock", Mark: 101, PrevDay: 95}}
	s.mu.Lock()
	s.carryForward(s.assets, assets)
	s.mu.Unlock()

	if assets["xyz:NVDA"].Pineify == nil || assets["xyz:NVDA"].Pineify.Coverage != "full" {
		t.Fatalf("within-window snapshot should be carried intact: %+v", assets["xyz:NVDA"].Pineify)
	}
}

// ---------------------------------------------------------------------------
// F2: Pineify rate is clamped to the confirmed 30/min ceiling
// ---------------------------------------------------------------------------

func TestPineifyRateClampedTo30(t *testing.T) {
	// Env 40 (above the confirmed ceiling) must clamp to 30.
	t.Setenv("PINEIFY_RATE_PER_MINUTE", "40")
	if cfg := LoadConfig(); cfg.PineifyRatePerMinute != 30 {
		t.Errorf("rate with env 40 = %d, want 30 (clamped to confirmed ceiling)", cfg.PineifyRatePerMinute)
	}
	// Env 1 (below floor) must clamp to 1.
	t.Setenv("PINEIFY_RATE_PER_MINUTE", "1")
	if cfg := LoadConfig(); cfg.PineifyRatePerMinute != 1 {
		t.Errorf("rate with env 1 = %d, want 1 (clamped to floor)", cfg.PineifyRatePerMinute)
	}
	// Env 25 stays.
	t.Setenv("PINEIFY_RATE_PER_MINUTE", "25")
	if cfg := LoadConfig(); cfg.PineifyRatePerMinute != 25 {
		t.Errorf("rate with env 25 = %d, want 25", cfg.PineifyRatePerMinute)
	}
	// Unset -> default 20 (within [1,30]).
	t.Setenv("PINEIFY_RATE_PER_MINUTE", "")
	if cfg := LoadConfig(); cfg.PineifyRatePerMinute != 20 {
		t.Errorf("default rate = %d, want 20", cfg.PineifyRatePerMinute)
	}
}

// ---------------------------------------------------------------------------
// Candidate priority
// ---------------------------------------------------------------------------

func TestSetPriorityDedup(t *testing.T) {
	s := testService(nil)
	s.SetPriority([]string{"xyz:AAPL", "xyz:AAPL", " xyz:MU ", ""})
	got := s.Priority()
	if len(got) != 2 {
		t.Fatalf("priority len = %d, want 2 (dedup + trim), got %v", len(got), got)
	}
	if got[0] != "xyz:AAPL" || got[1] != "xyz:MU" {
		t.Errorf("priority = %v, want [xyz:AAPL xyz:MU]", got)
	}
}

func TestSetPriorityClears(t *testing.T) {
	s := testService(nil)
	s.SetPriority([]string{"xyz:AAPL"})
	s.SetPriority(nil)
	if len(s.Priority()) != 0 {
		t.Fatalf("priority should be cleared, got %v", s.Priority())
	}
}

// ---------------------------------------------------------------------------
// F6: priority tier is bounded so crypto/TradeFi tiers are not starved, and
// the score-ordered tiers order by a real composite (not inert zero Scores)
// ---------------------------------------------------------------------------

func TestEnrichmentOrderPriorityCapped(t *testing.T) {
	priority := []string{"xyz:A", "xyz:B", "xyz:C", "xyz:D", "xyz:E", "xyz:F", "xyz:G", "xyz:H", "xyz:I", "xyz:J", "xyz:K", "xyz:L"}
	crypto := []string{"BTC", "ETH", "SOL"}
	mappable := []string{"xyz:NVDA", "xyz:AAPL"}
	byScore := []string{"xyz:NVDA", "xyz:AAPL"}
	budget := 6

	ordered := buildEnrichmentOrder(priority, crypto, mappable, byScore, budget)
	pos := func(s string) int {
		for i, x := range ordered {
			if x == s {
				return i
			}
		}
		return -1
	}

	// The priority tier must be capped at K = min(10, 6) = 6, not all 12, so
	// the first 6 slots are exactly the first 6 engine candidates.
	for i := 0; i < 6; i++ {
		if p := pos(priority[i]); p != i {
			t.Errorf("priority candidate %q should be at slot %d, got %d (%v)", priority[i], i, p, ordered)
		}
	}
	// The crypto floor must not be starved by a large candidate set.
	if pos("BTC") < 0 || pos("ETH") < 0 {
		t.Fatalf("crypto floor missing from order: %v", ordered)
	}
	// Candidates beyond the cap must not preempt the crypto floor.
	for _, sym := range priority[6:] {
		if p := pos(sym); p != -1 && p < pos("BTC") {
			t.Errorf("candidate %q beyond cap should be ordered after the crypto floor, got slot %d (%v)", sym, p, ordered)
		}
	}
}

// TestBoardScoreOrdering verifies the score-ordered tiers order by a real
// recomputed composite (F6): on the fresh local assets map every a.Score is 0,
// so ordering must come from cohortComposite (boardScore), not the inert field.
func TestBoardScoreOrdering(t *testing.T) {
	s := &Service{}
	assets := map[string]*asset{
		"xyz:NVDA": {Symbol: "xyz:NVDA", MarketType: "hip3_perp", Mark: 120, PrevDay: 100, Funding: 0.001},
		"xyz:AAPL": {Symbol: "xyz:AAPL", MarketType: "hip3_perp", Mark: 110, PrevDay: 100, Funding: 0.001},
		"xyz:MSFT": {Symbol: "xyz:MSFT", MarketType: "hip3_perp", Mark: 95, PrevDay: 100, Funding: -0.001},
		"xyz:MU":   {Symbol: "xyz:MU", MarketType: "hip3_perp", Mark: 100, PrevDay: 100, Funding: -0.002},
	}
	score := s.boardScore(assets)
	if score["xyz:NVDA"] <= score["xyz:AAPL"] {
		t.Errorf("NVDA (highest 24h delta) should rank above AAPL by composite: %v", score)
	}
	out := s.mappableBoard(assets, score)
	if len(out) != 4 {
		t.Fatalf("mappableBoard len = %d, want 4", len(out))
	}
	for i := 1; i < len(out); i++ {
		if score[out[i-1]] < score[out[i]] {
			t.Errorf("mappableBoard not sorted desc by composite: %v (scores %v)", out, score)
			break
		}
	}
	if out[0] != "xyz:NVDA" {
		t.Errorf("strongest by composite should be first, got %v", out)
	}
}

// ---------------------------------------------------------------------------
// F8: wall-clock deadline bounds synchronous enrichment
// ---------------------------------------------------------------------------

// TestEnrichSymbolDeadlineStops verifies enrichSymbol stops between tiers once
// the wall-clock deadline has passed, without making any further calls.
func TestEnrichSymbolDeadlineStops(t *testing.T) {
	cfg := &Config{PineifyMCPToken: "x", PineifyRatePerMinute: 100}
	s := &Service{cfg: cfg}
	client := newPineifyClient(cfg, nil, nil)
	limiter := newTokenBucket(100)
	// stopAt already in the past => must stop before any Pineify call.
	snap, calls := s.enrichSymbol(context.Background(), client, limiter, "xyz:NVDA", "hip3_perp", 10, time.Now().Add(-time.Second))
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (deadline already passed)", calls)
	}
	if snap == nil || snap.Error != "deadline reached" {
		t.Errorf("snapshot = %+v, want Error=deadline reached", snap)
	}
	if snap.Coverage != "" {
		t.Errorf("coverage = %q, want empty", snap.Coverage)
	}
}

// TestEnrichmentStopsAtDeadline runs the full pipeline with a very short
// deadline and a slow fake Pineify, and verifies the loop stops early instead
// of consuming the whole budget.
func TestEnrichmentStopsAtDeadline(t *testing.T) {
	srv := newFakeMCP()
	srv.delay = 30 * time.Millisecond
	ts := newTestHTTPServer(srv.handle())
	defer ts.Close()
	cfg := &Config{
		PineifyMCPToken:      "test-token",
		PineifyBaseURL:       ts.URL,
		PineifyRatePerMinute: 120, // ~0.5s spacing; latency is dominated by the fake's delay
	}
	// A ~1ms deadline forces the loop to stop almost immediately.
	s := &Service{cfg: cfg, httpClient: ts.Client(), pineifyDeadline: time.Millisecond}
	assets := map[string]*asset{
		"xyz:NVDA": {Symbol: "xyz:NVDA", MarketType: "hip3_perp", Score: 90},
		"xyz:AAPL": {Symbol: "xyz:AAPL", MarketType: "hip3_perp", Score: 80},
		"xyz:MU":   {Symbol: "xyz:MU", MarketType: "hip3_perp", Score: 70},
	}
	s.enrichWithPineify(context.Background(), assets)

	srv.mu.Lock()
	taCalls := srv.toolCalls["get-technical-analysis-snapshot"]
	srv.mu.Unlock()
	// 3 symbols × (TA+events+rating) would be many calls if the whole budget
	// were consumed; the deadline must stop the loop well short of that.
	if taCalls >= 3 {
		t.Errorf("deadline not enforced: %d TA calls made, want < 3 (loop should stop early)", taCalls)
	}
}
