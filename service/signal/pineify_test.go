package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// Rate limiter
// ---------------------------------------------------------------------------

func TestTokenBucketAcquire(t *testing.T) {
	b := newTokenBucket(5) // 5/min
	ctx := context.Background()
	// Burst of 5 should be immediate.
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := b.acquire(ctx); err != nil {
			t.Fatalf("unexpected acquire error: %v", err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatalf("burst of 5 should be near-instant, took %v", time.Since(start))
	}
	// 6th should wait ~ a refill period.
	start = time.Now()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("6th acquire errored: %v", err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("6th acquire should have waited for a refill, took %v", time.Since(start))
	}
}

func TestTokenBucketCancellation(t *testing.T) {
	b := newTokenBucket(1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("first acquire errored: %v", err)
	}
	if err := b.acquire(ctx); err != context.DeadlineExceeded {
		t.Fatalf("second acquire should have been cancelled, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Enrichment / degraded handling via httptest MCP server
// ---------------------------------------------------------------------------

// fakeMCP is a minimal fake MCP JSON-RPC server for tests. It returns canned
// tool results and records calls.
type fakeMCP struct {
	mu     sync.Mutex
	calls  map[string]int
	taJSON string
	events string
	rating string
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{
		calls:  make(map[string]int),
		taJSON: `{"snapshots":[{"timeframe":"1d","values":{"signal":"bullish","score":82,"rsi14":62,"adx14":28}}]}`,
		events: `{"data":{"upcomingEvents":[{"eventType":"earnings","date":"2026-08-26","epsEstimate":2.08}],"recentEvents":[{"eventType":"news","title":"NVDA news","eventAt":"2026-08-14T20:02:10.000Z"}]}}`,
		rating: `{"instrument":{"symbol":"NVDA"},"rating":{"overallRank":295,"scores":{"overall":8,"change":0,"fundamental":9,"technical":3}}}`,
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
	snap, _ := s.enrichSymbol(ctx, client, limiter, "xyz:NVDA", "hip3_perp", 10)
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

func TestEnrichSymbolNotMappable(t *testing.T) {
	cfg := &Config{PineifyMCPToken: "x", PineifyRatePerMinute: 10}
	s := &Service{cfg: cfg}
	client := newPineifyClient(cfg, nil, nil)
	limiter := newTokenBucket(10)
	snap, _ := s.enrichSymbol(context.Background(), client, limiter, "xyz:SP500", "hip3_perp", 10)
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
	snap, _ := s.enrichSymbol(ctx, client, limiter, "xyz:NVDA", "hip3_perp", 10)
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
		Dimensions []map[string]any `json:"dimensions"`
		CompositeZ string           `json:"compositeZ"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Dimensions) != 6 {
		t.Fatalf("dimensions = %d, want 6 (3 core + 3 pineify)", len(out.Dimensions))
	}
	// Last row should be the Analyst/Pineify Rating row with percentile filled.
	last := out.Dimensions[len(out.Dimensions)-1]
	if last["family"] != "Analyst" {
		t.Errorf("last family = %v, want Analyst", last["family"])
	}
	if last["percentile"] == "-" || last["percentile"] == nil || last["percentile"] == "" {
		t.Errorf("pineify row percentile should be filled, got %v", last["percentile"])
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
