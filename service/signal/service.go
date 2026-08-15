package signal

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

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
	Score       float64 // last composite z-score (populated by Rank; 0 until ranked)
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

	httpClient *http.Client

	mu         sync.RWMutex
	assets     map[string]*asset // by Symbol
	order      []string          // stable ordering for cross-sectional reduction
	lastIngest time.Time
	ingestErr  error
}

// NewService constructs a Service from config.
func NewService(cfg *Config) *Service {
	return &Service{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		assets:     make(map[string]*asset),
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
	// Carry OI-delta forward for symbols present in both snapshots.
	for sym, a := range assets {
		if prev, ok := s.assets[sym]; ok {
			a.OIPrev = prev.OI
		}
	}
	s.assets = assets
	s.order = order
	s.lastIngest = time.Now()
	s.ingestErr = nil
	s.mu.Unlock()
	return nil
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
