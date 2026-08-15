package signal

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Real Hyperliquid taker-flow + order-book liquidity data plane (Option B).
//
// This file owns a single gorilla/websocket connection to the public HL WS
// (`wss://api.hyperliquid.xyz/ws`) carrying many coin subscriptions. A
// read-loop goroutine is the only reader; a subscription-manager loop is the
// only writer. The manager lazily subscribes to the engine's current candidate
// set (Service.prioritySymbols via SetPriority) plus recently-touched
// dashboard symbols, reconnects with exponential backoff on any read/dial
// error, re-subscribes the full desired set on reconnect, and GCs idle
// touched symbols. The read-loop accumulates each batched `trades`/`l2Book`
// frame locally, then merges it into Service.flow under the single Service
// mutex (short critical sections; no nested locks).

const (
	// wsDefaultURL is the public Hyperliquid WebSocket base (matches the
	// wss://api.hyperliquid.xyz/ws used by web/src/components/terminal/OrderBook.tsx).
	wsDefaultURL = "wss://api.hyperliquid.xyz/ws"
	// wsReadTimeout is the read deadline; a pong or any frame resets it, so a
	// silent socket is recycled.
	wsReadTimeout = 90 * time.Second
	// wsBackoffBase / wsBackoffCap bound the exponential reconnect backoff.
	wsBackoffBase = 100 * time.Millisecond
	wsBackoffCap  = 30 * time.Second
	// wsMaxSubscriptions bounds the lazy subscription set (priority wins).
	wsMaxSubscriptions = 100
)

// flowBin is one price bin of accumulated notional with its last touch time
// (read-time exponential decay; no timers).
type flowBin struct {
	notional  float64
	lastTouch time.Time
}

// SymbolFlow is the per-symbol taker-flow + resting-depth accumulator. It is
// keyed by the asset.Symbol coin id and guarded by Service.mu.
//
// buyByBin/sellByBin hold aggressive taker-buy/sell notional bucketed by the
// anchor offset d = round((px - anchorMark)/anchorStep) clamped to
// +-heatmapHalfRange. bidScope/askScope hold near-touch resting depth (the
// l2Book gives only ~20 levels/side) in the same bin space; bidTotal/askTotal
// are their USD totals. anchorMark/anchorStep are captured at (re)subscribe;
// Heatmap() re-bins by absolute price so mark drift between ingests is handled.
type SymbolFlow struct {
	Coin       string
	anchorMark float64
	anchorStep float64
	buyByBin   map[int]flowBin
	sellByBin  map[int]flowBin
	bidScope   map[int]flowBin
	askScope   map[int]flowBin
	bidTotal   float64
	askTotal   float64

	subscribedAt time.Time
	maxTouch     time.Time
}

// ─────────────────────────────────────────────────────────────────────────────
// Hyperliquid WS message shapes (mirrors web/src/components/terminal/OrderBook.tsx)
// ─────────────────────────────────────────────────────────────────────────────

// flexFloat accepts a JSON number or numeric string; HL sends px/sz as strings.
type flexFloat float64

// UnmarshalJSON parses either a JSON number or a numeric string into a float64.
func (f *flexFloat) UnmarshalJSON(b []byte) error {
	var num float64
	if err := json.Unmarshal(b, &num); err == nil {
		*f = flexFloat(num)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// wsTrade is one taker-trade print. side "B" = aggressive buy (long cost),
// "A" = aggressive sell (short cost); "C"/"T" are close/liquidation-only and
// are ignored (not directional flow).
type wsTrade struct {
	Coin string    `json:"coin"`
	Side string    `json:"side"`
	Px   flexFloat `json:"px"`
	Sz   flexFloat `json:"sz"`
}

type wsTradesFrame struct {
	Channel string    `json:"channel"`
	Data    []wsTrade `json:"data"`
}

type wsLevel struct {
	Px flexFloat `json:"px"`
	Sz flexFloat `json:"sz"`
}

type wsBookFrame struct {
	Channel string `json:"channel"`
	Data    struct {
		Coin   string      `json:"coin"`
		Levels [][]wsLevel `json:"levels"` // [0]=bids, [1]=asks
		Time   int64       `json:"time"`
	} `json:"data"`
}

type wsSubscribeMsg struct {
	Method       string        `json:"method"`
	Subscription wsSubcription `json:"subscription"`
}

type wsSubcription struct {
	Type string `json:"type"`
	Coin string `json:"coin"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Subscription manager (single writer) + reader loop (single reader)
// ─────────────────────────────────────────────────────────────────────────────

// wsManager is the WS data-plane owner. It runs in a Service goroutine started
// by Start(); it dials, lazily reconciles the desired subscription set on
// wake/ticker, and reconnects with backoff on read errors. Exits on ctx.Done().
func (s *Service) wsManager(ctx context.Context) {
	if s.cfg == nil || !s.cfg.WSEnabled {
		return
	}
	var (
		conn         *websocket.Conn
		acked        = map[string]bool{}
		backoff      = wsBackoffBase
		readErr      = make(chan struct{}, 1)
		reconcileTic = time.NewTicker(s.wsReconcileInterval())
	)
	defer reconcileTic.Stop()

	readLoop := func(c *websocket.Conn) {
		for {
			_ = c.SetReadDeadline(time.Now().Add(wsReadTimeout))
			_, msg, err := c.ReadMessage()
			if err != nil {
				select {
				case readErr <- struct{}{}:
				default:
				}
				return
			}
			s.handleWSMessage(msg)
		}
	}

	dial := func() {
		url := wsDefaultURL
		if s.cfg != nil && s.cfg.HLWSURL != "" {
			url = s.cfg.HLWSURL
		}
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			s.log.Warnf("hl ws dial failed: %v", err)
			return
		}
		c.SetReadLimit(1 << 20)
		conn = c
		acked = map[string]bool{}
		backoff = wsBackoffBase
		go readLoop(c)
	}

	reconcile := func() {
		if conn == nil {
			return
		}
		desired := s.desiredSubscriptions()
		for _, coin := range desired {
			if acked[coin] {
				continue
			}
			s.ensureFlow(coin)
			s.subscribeCoin(conn, coin)
			acked[coin] = true
		}
		for coin := range acked {
			if containsString(desired, coin) {
				continue
			}
			s.unsubscribeCoin(conn, coin)
			delete(acked, coin)
			s.dropFlow(coin)
		}
	}

	dial()
	for {
		reconcile()
		select {
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
			return
		case <-s.priorityWake:
		case <-reconcileTic.C:
		case <-readErr:
			if conn != nil {
				_ = conn.Close()
			}
			conn = nil
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < wsBackoffCap {
				backoff *= 2
			}
			dial()
		}
	}
}

// handleWSMessage routes one WS frame: batched trades -> flow accumulator,
// l2Book -> depth accumulator, anything else ignored. Runs in the reader loop.
func (s *Service) handleWSMessage(msg []byte) {
	var env struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return
	}
	switch env.Channel {
	case "trades":
		var fr wsTradesFrame
		if err := json.Unmarshal(msg, &fr); err != nil {
			return
		}
		byCoin := map[string][]wsTrade{}
		for _, tr := range fr.Data {
			if tr.Side != "B" && tr.Side != "A" {
				continue // C (close) / T (liquidation) are not directional flow
			}
			byCoin[tr.Coin] = append(byCoin[tr.Coin], tr)
		}
		for coin, fills := range byCoin {
			s.mergeTradesFrame(coin, fills)
		}
	case "l2Book":
		var fr wsBookFrame
		if err := json.Unmarshal(msg, &fr); err != nil {
			return
		}
		var bids, asks []wsLevel
		if len(fr.Data.Levels) >= 2 {
			bids = fr.Data.Levels[0]
			asks = fr.Data.Levels[1]
		}
		s.mergeBookFrame(fr.Data.Coin, bids, asks)
	}
}

func (s *Service) subscribeCoin(c *websocket.Conn, coin string) {
	_ = s.writeWS(c, wsSubscribeMsg{Method: "subscribe", Subscription: wsSubcription{Type: "trades", Coin: coin}})
	_ = s.writeWS(c, wsSubscribeMsg{Method: "subscribe", Subscription: wsSubcription{Type: "l2Book", Coin: coin}})
}

func (s *Service) unsubscribeCoin(c *websocket.Conn, coin string) {
	_ = s.writeWS(c, wsSubscribeMsg{Method: "unsubscribe", Subscription: wsSubcription{Type: "trades", Coin: coin}})
	_ = s.writeWS(c, wsSubscribeMsg{Method: "unsubscribe", Subscription: wsSubcription{Type: "l2Book", Coin: coin}})
}

func (s *Service) writeWS(c *websocket.Conn, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(websocket.TextMessage, b)
}

// ─────────────────────────────────────────────────────────────────────────────
// Lazy subscription set + lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// desiredSubscriptions returns the current lazy target set: engine candidates
// (priority) first, then recently-touched dashboard symbols, capped at
// wsMaxSubscriptions (priority wins under pressure).
func (s *Service) desiredSubscriptions() []string {
	s.mu.RLock()
	pri := make([]string, len(s.prioritySymbols))
	copy(pri, s.prioritySymbols)
	touched := map[string]bool{}
	now := s.now()
	ttl := s.wsSiteIdleTTL()
	for sym, t := range s.touchedBy {
		if now.Sub(t) <= ttl {
			touched[sym] = true
		}
	}
	s.mu.RUnlock()

	seen := map[string]bool{}
	var out []string
	for _, sym := range pri {
		sym = strings.TrimSpace(sym)
		if sym == "" || seen[sym] {
			continue
		}
		if len(out) >= wsMaxSubscriptions {
			break
		}
		seen[sym] = true
		out = append(out, sym)
	}
	for sym := range touched {
		if seen[sym] || len(out) >= wsMaxSubscriptions {
			continue
		}
		seen[sym] = true
		out = append(out, sym)
	}
	return out
}

// TouchFlowSymbol records a dashboard heatmap request for a symbol so the WS
// manager keeps it subscribed (tier-2 lazy) until it idles past WSSiteIdleTTL.
func (s *Service) TouchFlowSymbol(symbol string) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return
	}
	s.mu.Lock()
	s.touchedBy[symbol] = s.now()
	s.mu.Unlock()
	s.wakeWS()
}

// wakeWS non-blocking-notifies the WS manager to re-reconcile.
func (s *Service) wakeWS() {
	select {
	case s.priorityWake <- struct{}{}:
	default:
	}
}

// ensureFlow initializes the flow accumulator for a coin (no-op if present),
// capturing the anchor mark/step from the current snapshot so trades/levels can
// be bucketed to absolute-price bins (rendered later via rebin). Called by the
// manager outside any lock; assetFor reads under its own lock.
func (s *Service) ensureFlow(coin string) {
	var anchorMark, anchorStep float64
	if a, ok := s.assetFor(coin); ok {
		anchorMark = a.Mark
		anchorStep = heatmapBinStep(a)
		if anchorStep <= 0 {
			anchorStep = 0.01 * anchorMark
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.flow[coin]; ok {
		return
	}
	s.flow[coin] = &SymbolFlow{
		Coin:         coin,
		anchorMark:   anchorMark,
		anchorStep:   anchorStep,
		buyByBin:     map[int]flowBin{},
		sellByBin:    map[int]flowBin{},
		bidScope:     map[int]flowBin{},
		askScope:     map[int]flowBin{},
		subscribedAt: s.now(),
	}
}

// dropFlow removes a symbol's accumulator + touch record after unsubscribe/GC.
func (s *Service) dropFlow(coin string) {
	s.mu.Lock()
	delete(s.flow, coin)
	delete(s.touchedBy, coin)
	s.mu.Unlock()
}

// ─────────────────────────────────────────────────────────────────────────────
// Accumulator merges (run in the reader loop, under Service.mu)
// ─────────────────────────────────────────────────────────────────────────────

// mergeTradesFrame merges one batched trades frame for a coin. A batch is
// accumulated into the flow under a single lock acquisition.
func (s *Service) mergeTradesFrame(coin string, fills []wsTrade) {
	if len(fills) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flow[coin]
	if f == nil {
		return // not subscribed / already dropped
	}
	now := s.now()
	for _, tr := range fills {
		notional := float64(tr.Px) * float64(tr.Sz)
		if notional <= 0 || tr.Px <= 0 {
			continue
		}
		d := flowOffset(float64(tr.Px), f.anchorMark, f.anchorStep)
		if tr.Side == "B" {
			mergeBin(&f.buyByBin, d, notional, now)
		} else if tr.Side == "A" {
			mergeBin(&f.sellByBin, d, notional, now)
		}
		if now.After(f.maxTouch) {
			f.maxTouch = now
		}
	}
}

// mergeBookFrame replaces a coin's near-touch resting-depth scope from a full
// l2Book snapshot (levels[0]=bids, levels[1]=asks), recomputing the USD totals.
func (s *Service) mergeBookFrame(coin string, bids, asks []wsLevel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flow[coin]
	if f == nil {
		return
	}
	now := s.now()
	f.bidTotal, f.askTotal = 0, 0
	for k := range f.bidScope {
		delete(f.bidScope, k)
	}
	for k := range f.askScope {
		delete(f.askScope, k)
	}
	for _, lv := range bids {
		notional := float64(lv.Px) * float64(lv.Sz)
		if notional <= 0 || lv.Px <= 0 {
			continue
		}
		d := flowOffset(float64(lv.Px), f.anchorMark, f.anchorStep)
		b := f.bidScope[d]
		b.notional += notional
		b.lastTouch = now
		f.bidScope[d] = b
		f.bidTotal += notional
	}
	for _, lv := range asks {
		notional := float64(lv.Px) * float64(lv.Sz)
		if notional <= 0 || lv.Px <= 0 {
			continue
		}
		d := flowOffset(float64(lv.Px), f.anchorMark, f.anchorStep)
		b := f.askScope[d]
		b.notional += notional
		b.lastTouch = now
		f.askScope[d] = b
		f.askTotal += notional
	}
	if now.After(f.maxTouch) {
		f.maxTouch = now
	}
}

// mergeBin accumulates notional into a bin map at offset d.
func mergeBin(m *map[int]flowBin, d int, notional float64, now time.Time) {
	if *m == nil {
		*m = make(map[int]flowBin)
	}
	b := (*m)[d]
	b.notional += notional
	b.lastTouch = now
	(*m)[d] = b
}

// flowOffset maps an absolute price to the anchor bin offset, clamped to the
// ladder range. A zero/absent anchor falls back to the mark bin (0).
func flowOffset(px, anchorMark, anchorStep float64) int {
	if anchorMark <= 0 || anchorStep <= 0 {
		return 0
	}
	d := int(math.Round((px - anchorMark) / anchorStep))
	if d > heatmapHalfRange {
		d = heatmapHalfRange
	}
	if d < -heatmapHalfRange {
		d = -heatmapHalfRange
	}
	return d
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// Read-time decay + config-derived helpers (used by Heatmap())
// ─────────────────────────────────────────────────────────────────────────────

// flowMaxAge returns how long a flow entry stays fresh (>= this => stale =>
// Heatmap falls back to the OI x mark formula).
func (s *Service) flowMaxAge() time.Duration {
	if s.cfg != nil && s.cfg.FlowMaxAge > 0 {
		return s.cfg.FlowMaxAge
	}
	return 2 * time.Hour
}

// decayedBin returns the time-decayed notional of a flow bin at offset d,
// using the configured half-life and max-age, read at time `now`. Old prints
// fade exponentially; beyond max-age they read as 0 (so the heatmap falls
// back). Read-time decay keeps renders idempotent within a timestamp window.
func (s *Service) decayedBin(m map[int]flowBin, d int, now time.Time) float64 {
	b, ok := m[d]
	if !ok || b.notional <= 0 {
		return 0
	}
	age := now.Sub(b.lastTouch)
	if age <= 0 {
		return b.notional
	}
	if age > s.flowMaxAge() {
		return 0
	}
	hl := 30 * time.Minute
	if s.cfg != nil && s.cfg.FlowDecayHalfLife > 0 {
		hl = s.cfg.FlowDecayHalfLife
	}
	return b.notional * math.Exp(-math.Ln2*age.Seconds()/hl.Seconds())
}

// wsBookWeight returns the factor blending near-touch resting depth into cost.
func (s *Service) wsBookWeight() float64 {
	if s.cfg != nil && s.cfg.WSBookWeight > 0 {
		return s.cfg.WSBookWeight
	}
	return 0.5
}

// wsReconcileInterval returns the WS manager reconcile/GC cadence.
func (s *Service) wsReconcileInterval() time.Duration {
	if s.cfg != nil && s.cfg.WSReconcileInterval > 0 {
		return s.cfg.WSReconcileInterval
	}
	return 30 * time.Second
}

// wsSiteIdleTTL returns how long a touched dashboard symbol stays subscribed
// before the manager unsubscribes + GCs it.
func (s *Service) wsSiteIdleTTL() time.Duration {
	if s.cfg != nil && s.cfg.WSSiteIdleTTL > 0 {
		return s.cfg.WSSiteIdleTTL
	}
	return 15 * time.Minute
}
