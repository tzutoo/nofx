package signal

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the standalone signal service.
// Everything is env-driven so the same binary runs as a container or bare.
type Config struct {
	// Listen is the HTTP listen address (default ":8480").
	Listen string
	// Interval is how often the ingest worker polls Hyperliquid (default 3m,
	// must be <= the engine's min scan cadence so the cache is never stale
	// relative to a trading cycle).
	Interval time.Duration
	// HyperliquidWalletAddr, when set, enables real self-wallet book-PnL and
	// liquidation anchors in the heatmap / signal-lab products.
	HyperliquidWalletAddr string
	// PineifyMCPToken gates the optional Pineify augmentation. Never logged.
	PineifyMCPToken string
	// PineifyBaseURL is the Pineify MCP endpoint.
	PineifyBaseURL string
	// PineifyRatePerMinute bounds external Pineify calls; enforced in the
	// ingest worker only, never on the request path (Pineify rate-limits to a
	// few calls per minute). Default 20/min, clamped to [1,30].
	PineifyRatePerMinute int
	// PineifyBoostEnabled enables the opt-in ranking qualifier: when a mappable
	// item with full Pineify coverage strongly opposes the HL bias, it is
	// demoted (or excluded with PineifyHardReject). Default off (overlay-only).
	PineifyBoostEnabled bool
	// PineifyHardReject, only with boost enabled, excludes (rather than just
	// demotes) an item whose full-coverage Pineify bias opposes the HL bias.
	PineifyHardReject bool
	// PineifyMinConviction is the conviction threshold for demote/reject.
	PineifyMinConviction float64
	// DefaultAvgLeverage is the assumed average leverage used when inferring
	// exposure proxies from open interest (informational only).
	DefaultAvgLeverage float64

	// WSEnabled gates the real Hyperliquid WebSocket taker-flow + l2Book data
	// plane (Option B). When disabled, the heatmap falls back to the synthetic
	// OI x mark formula. Default true; zero-symbol sets keep it cheap.
	WSEnabled bool
	// HLWSURL is the Hyperliquid public WebSocket base.
	HLWSURL string
	// WSReconcileInterval is how often the WS manager re-diffs the desired
	// subscription set against the live set and GCs idle/touched symbols.
	WSReconcileInterval time.Duration
	// FlowDecayHalfLife is the read-time exponential half-life of taker-flow
	// (aggressive buy/sell) notional per price bin. Old prints fade; beyond
	// FlowMaxAge the flow is treated as stale and the heatmap falls back.
	FlowDecayHalfLife time.Duration
	// FlowMaxAge is how long a flow entry stays fresh before the heatmap falls
	// back to the OI x mark formula (>= this age = stale).
	FlowMaxAge time.Duration
	// WSSiteIdleTTL is how long a touched (dashboard) symbol stays subscribed
	// after its last heatmap touch before it is unsubscribed and GC'd.
	WSSiteIdleTTL time.Duration
	// WSBookWeight is the factor blending real near-touch resting depth into
	// the cost layer (longCost += bookWeight * ask-depth at that bin). 0..1.
	WSBookWeight float64
}

// LoadConfig reads config from the environment, applying defaults.
func LoadConfig() *Config {
	return &Config{
		Listen:                env("SIGNAL_SERVICE_LISTEN", ":8480"),
		Interval:              envDuration("SIGNAL_SERVICE_INTERVAL", 3*time.Minute),
		HyperliquidWalletAddr: env("HYPERLIQUID_WALLET_ADDR", ""),
		PineifyMCPToken:       env("PINEIFY_MCP_TOKEN", ""),
		PineifyBaseURL:        env("PINEIFY_BASE_URL", "https://agents.pineify.app/mcp"),
		PineifyRatePerMinute:  clampInt(envInt("PINEIFY_RATE_PER_MINUTE", 20), 1, 30),
		PineifyBoostEnabled:   envBool("PINEIFY_BOOST_ENABLED", false),
		PineifyHardReject:     envBool("PINEIFY_HARD_REJECT", false),
		PineifyMinConviction:  envFloat("PINEIFY_MIN_CONVICTION", 0.7),
		DefaultAvgLeverage:    envFloat("SIGNAL_SERVICE_AVG_LEVERAGE", 10),

		WSEnabled:           envBool("SIGNAL_SERVICE_WS_ENABLED", true),
		HLWSURL:             env("HYPERLIQUID_WS_URL", "wss://api.hyperliquid.xyz/ws"),
		WSReconcileInterval: envDuration("SIGNAL_SERVICE_WS_RECONCILE_INTERVAL", 30*time.Second),
		FlowDecayHalfLife:   envDuration("SIGNAL_SERVICE_WS_DECAY", 30*time.Minute),
		FlowMaxAge:          envDuration("SIGNAL_SERVICE_WS_MAX_AGE", 2*time.Hour),
		WSSiteIdleTTL:       envDuration("SIGNAL_SERVICE_WS_IDLE_TTL", 15*time.Minute),
		WSBookWeight:        clampFloat(envFloat("SIGNAL_SERVICE_WS_BOOK_WEIGHT", 0.5), 0, 1),
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
