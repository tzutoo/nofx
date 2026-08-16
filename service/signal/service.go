package signal

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/mcp"
	"nofx/provider/hyperliquid"
)

// asset is the per-symbol cross-section the service ingests and reduces into
// signal products. OI holds the previous poll's open interest so OI-delta can
// be computed (a ranking factor); it is zero until the second ingest.
type asset struct {
	Symbol      string  // canonical symbol, e.g. "BTC" (core) or "xyz:NVDA" (hip3)
	MarketType  string  // "core_perp" for crypto, "hip3_perp" for xyz/TradeFi
	Category    string  // "crypto" or a stock/commodity/index/forex category
	Mark        float64 // mark price
	PrevDay     float64 // previous-day reference price (24h delta)
	Funding     float64 // current funding rate
	OI          float64 // current open interest (base-coin units)
	OIPrev      float64 // previous poll open interest
	Oracle      float64 // oracle price
	Score       float64 // composite z-score vestige; NOT written by Rank (F1: mutating the shared snapshot pointer would race HTTP handlers). Only referenced by the inert pre-Rank enrichment board sort.
	MaxLeverage int
	SzDecimals  int
	Pineify     *PineifySnapshot // optional Pineify enrichment overlay (nil unless token set)
}

func (a *asset) OIDelta() float64 { return a.OI - a.OIPrev }

// Service is the standalone signal-service coordinator. A single ingest
// goroutine polls Hyperliquid into an immutable snapshot; HTTP handlers read
// the snapshot and compute products on demand (cheap cross-sectional
// reductions), caching each product for the ingest interval.
type Service struct {
	cfg *Config
	log mcp.Logger

	httpClient *http.Client

	mu         sync.RWMutex
	assets     map[string]*asset // by Symbol
	order      []string          // stable ordering for cross-sectional reduction
	lastIngest time.Time
	ingestErr  error
	// prioritySymbols is the engine's current candidate set (symbols it is
	// actively evaluating). Enrichment prioritizes these so candidates carry
	// fresh Pineify data at decision time. Thread-safe; set via SetPriority.
	prioritySymbols []string
	// pineifyDeadline is a test override for the synchronous-enrichment
	// wall-clock bound (F8). Zero computes it from the ingest interval.
	pineifyDeadline time.Duration

	// flow holds the per-symbol taker-flow + order-book liquidity accumulator
	// fed by the real Hyperliquid WS client (hlws.go). It is keyed by the same
	// asset.Symbol coin id and OUTLIVES snapshot swaps (Ingest replaces
	// s.assets wholesale but must NOT clear s.flow). Read by Heatmap() under
	// RLock; written by the WS reader under Lock.
	flow map[string]*SymbolFlow
	// now is the clock used by read-time flow decay and freshness. Defaults to
	// time.Now; tests override it to freeze determinism.
	now func() time.Time
	// priorityWake is a size-1 buffered channel the WS manager selects on so it
	// re-reconciles promptly when the engine's candidate set or the touched set
	// changes (SetPriority / TouchFlowSymbol send a non-blocking wake).
	priorityWake chan struct{}
	// touchedBy records the last heatmap touch per symbol (dashboard tier-2
	// lazy subscription source). Entries are GC'd when they idle past WSSiteIdleTTL.
	touchedBy map[string]time.Time
}

// NewService constructs a Service from config.
func NewService(cfg *Config) *Service {
	return &Service{
		cfg:          cfg,
		log:          logger.NewMCPLogger(),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		assets:       make(map[string]*asset),
		flow:         make(map[string]*SymbolFlow),
		now:          time.Now,
		priorityWake: make(chan struct{}, 1),
		touchedBy:    make(map[string]time.Time),
	}
}

// Config returns the service config.
func (s *Service) Config() *Config { return s.cfg }

// Snapshot returns a consistent read of the current asset cross-section under
// the read lock.
func (s *Service) Snapshot() (map[string]*asset, []string, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.assets, s.order, s.lastIngest, s.ingestErr
}

// SetPriority stores the engine's current candidate symbols so enrichment can
// prioritize them. It is safe for concurrent HTTP handlers and the ingest
// worker. Passing an empty list clears the priority set.
func (s *Service) SetPriority(symbols []string) {
	seen := make(map[string]struct{}, len(symbols))
	clean := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		sym = strings.TrimSpace(sym)
		if sym == "" {
			continue
		}
		if _, dup := seen[sym]; dup {
			continue
		}
		seen[sym] = struct{}{}
		clean = append(clean, sym)
	}
	s.mu.Lock()
	s.prioritySymbols = clean
	s.mu.Unlock()
	s.wakeWS()
}

// Priority returns the engine's current candidate symbols (may be nil/empty).
func (s *Service) Priority() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.prioritySymbols))
	copy(out, s.prioritySymbols)
	return out
}

// Ingest fetches the current Hyperliquid cross-section for both the default
// (crypto core_perp) and xyz (hip3_perp TradeFi) perp dexs, enriches the
// mappable subset with Pineify (when enabled), and swaps it into the snapshot,
// carrying OI-delta forward from the previous snapshot.
func (s *Service) Ingest(ctx context.Context) error {
	assets := make(map[string]*asset)
	var order []string

	// Default perp dex -> core_perp (crypto).
	if err := s.ingestDex(ctx, "", "core_perp", assets); err != nil {
		s.mu.Lock()
		s.ingestErr = err
		s.lastIngest = time.Now()
		s.mu.Unlock()
		return err
	}
	// xyz perp dex -> hip3_perp (TradeFi).
	if err := s.ingestDex(ctx, "xyz", "hip3_perp", assets); err != nil {
		s.mu.Lock()
		s.ingestErr = err
		s.lastIngest = time.Now()
		s.mu.Unlock()
		return err
	}

	// Enrich the mappable subset with Pineify overlays (rate-limited, gated by
	// token presence, never blocks the snapshot swap on failure).
	if s.pineifyEnabled() {
		s.enrichWithPineify(ctx, assets)
	}

	for sym := range assets {
		order = append(order, sym)
	}

	s.mu.Lock()
	// Carry OI-delta and last-known Pineify enrichment forward (see
	// carryForward). Freshly-enriched snapshots (this cycle) win; carried
	// snapshots older than the staleness window are dropped so stale ratings
	// are never rendered as current to a pre-entry gate.
	s.carryForward(s.assets, assets)
	s.assets = assets
	s.order = order
	s.lastIngest = time.Now()
	s.ingestErr = nil
	s.mu.Unlock()
	return nil
}

// pineifyStaleAfter is the age after which a carried Pineify overlay is
// considered stale (a reasonable multiple of the ingest interval). Beyond it a
// rating/event is too old to be rendered as current to a pre-entry gate.
func (s *Service) pineifyStaleAfter() time.Duration {
	iv := s.cfg.Interval
	if iv <= 0 {
		iv = 3 * time.Minute
	}
	return 6 * iv
}

// carryForward merges per-symbol state from the previous snapshot into the
// freshly-ingested assets: OI-delta (previous poll OI) and, for symbols the
// budget-limited enrichment did not refresh this cycle, the last-known Pineify
// overlay. Overlays older than the staleness window are carried as stale
// (Coverage cleared, Error="stale") so SignalLab never renders them as current.
func (s *Service) carryForward(prev, next map[string]*asset) {
	for sym, a := range next {
		p, ok := prev[sym]
		if !ok {
			continue
		}
		a.OIPrev = p.OI
		if a.Pineify != nil || p.Pineify == nil {
			continue
		}
		if time.Since(p.Pineify.FetchedAt) > s.pineifyStaleAfter() {
			stale := *p.Pineify
			stale.Coverage = ""
			stale.Error = "stale"
			a.Pineify = &stale
			continue
		}
		a.Pineify = p.Pineify
	}
}

// tradableSnapshot reports whether a market snapshot is live and tradeable. A
// delisted asset (Hyperliquid isDelisted flag) or one with no valid mark price
// would otherwise surface as a degenerate rank / heatmap / empty-dashboard
// entry, so both are excluded.
func tradableSnapshot(m hyperliquid.MarketSnapshot) bool {
	return !m.IsDelisted && m.MarkPx > 0
}

// ingestDex fetches one Hyperliquid dex and adds assets to the map, tagging
// market type and category.
func (s *Service) ingestDex(ctx context.Context, dex, marketType string, assets map[string]*asset) error {
	snap, err := hyperliquid.GetMarketSnapshot(ctx, s.httpClient, dex)
	if err != nil {
		return err
	}
	for _, m := range snap {
		symbol := m.Symbol
		if !tradableSnapshot(m) {
			// Delisted / no live market reports a zero mark price; skip it so it
			// never enters the snapshot, rank, or terminal (e.g. a delisted coin
			// with no order book, kline, or heatmap).
			continue
		}
		category := "crypto"
		if marketType == "hip3_perp" {
			base := strings.TrimPrefix(symbol, "xyz:")
			category = hyperliquid.XYZCategory(base)
		}
		assets[symbol] = &asset{
			Symbol:      symbol,
			MarketType:  marketType,
			Category:    category,
			Mark:        m.MarkPx,
			PrevDay:     m.PrevDayPx,
			Funding:     m.Funding,
			OI:          m.OpenInterest,
			Oracle:      m.OraclePx,
			MaxLeverage: m.MaxLeverage,
			SzDecimals:  m.SzDecimals,
		}
	}
	return nil
}

// Start runs the ingest worker on Config.Interval until ctx is cancelled. It
// ingests once immediately, then on a ticker, backing off on failure.
func (s *Service) Start(ctx context.Context) {
	// Immediate first ingest.
	if err := s.Ingest(ctx); err != nil {
		_ = err
	}
	// Spawn the WS manager (real taker-flow + l2Book) when enabled. It runs in
	// the service process, lazily subscribing to the engine's candidate set plus
	// recently-touched dashboard symbols, and exits on ctx cancellation.
	if s.cfg == nil || s.cfg.WSEnabled {
		go s.wsManager(ctx)
	}
	interval := s.cfg.Interval
	if interval <= 0 {
		interval = 3 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Ingest(ctx); err != nil {
				s.mu.Lock()
				s.ingestErr = err
				s.lastIngest = time.Now()
				s.mu.Unlock()
			}
		}
	}
}
