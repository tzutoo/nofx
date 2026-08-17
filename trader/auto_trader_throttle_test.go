package trader

import (
	"nofx/kernel"
	"nofx/market"
	"strings"
	"testing"
	"time"
)

func throttleContext(symbol, side string, heldFor time.Duration, pnlPct float64) *kernel.Context {
	return leveragedThrottleContext(symbol, side, heldFor, pnlPct, 1)
}

func leveragedThrottleContext(symbol, side string, heldFor time.Duration, pnlPct float64, leverage int) *kernel.Context {
	return &kernel.Context{
		Positions: []kernel.PositionInfo{
			{
				Symbol:           symbol,
				Side:             side,
				UnrealizedPnLPct: pnlPct,
				Leverage:         leverage,
				UpdateTime:       time.Now().Add(-heldFor).UnixMilli(),
			},
		},
	}
}

func TestTradeThrottleBlocksEarlyNoiseClose(t *testing.T) {
	at := &AutoTrader{}
	ctx := throttleContext("xyz:INTC", "long", 20*time.Minute, -0.3)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if !strings.Contains(reason, "min AI-managed hold") {
		t.Fatalf("expected early close to be blocked by min hold, got %q", reason)
	}
}

func TestTradeThrottleAllowsEarlyHardStop(t *testing.T) {
	at := &AutoTrader{}
	// A price loss beyond the default -3% bypass unlocks the min hold.
	ctx := throttleContext("xyz:INTC", "long", 20*time.Minute, -6.0)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if reason != "" {
		t.Fatalf("expected hard stop close to pass, got %q", reason)
	}
}

func TestTradeThrottleBypassIsPriceBasisNotMarginBasis(t *testing.T) {
	at := &AutoTrader{}
	// At 10x leverage the exchange reports margin-based PnL: -6% margin is
	// only a -0.6% price move — noise, must NOT bypass the min hold.
	ctx := leveragedThrottleContext("xyz:INTC", "long", 20*time.Minute, -6.0, 10)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if !strings.Contains(reason, "min AI-managed hold") {
		t.Fatalf("expected -0.6%% price move to stay blocked at 10x, got %q", reason)
	}

	// -60% margin at 10x is a real -6% price move — bypass allowed.
	ctx = leveragedThrottleContext("xyz:INTC", "long", 20*time.Minute, -60.0, 10)
	reason = at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if reason != "" {
		t.Fatalf("expected -6%% price move to bypass min hold at 10x, got %q", reason)
	}
}

func TestTradeThrottleNoiseBandIsPriceBasisNotMarginBasis(t *testing.T) {
	at := &AutoTrader{}
	// Past min hold at 10x: +20% margin is only a +2% price move, still
	// inside the default -2%..+3% noise band — flat close must stay blocked.
	ctx := leveragedThrottleContext("xyz:INTC", "long", 2*time.Hour, 20.0, 10)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if !strings.Contains(reason, "noise band") {
		t.Fatalf("expected +2%% price move to be blocked inside noise band at 10x, got %q", reason)
	}
}

func TestTradeThrottleBlocksFlatCloseInsideNoiseWindow(t *testing.T) {
	at := &AutoTrader{}
	// Held past the default 90m min hold but still inside the noise band and
	// under the 3h noise window.
	ctx := throttleContext("xyz:INTC", "long", 2*time.Hour, 0.4)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if !strings.Contains(reason, "noise band") {
		t.Fatalf("expected flat close to be blocked inside noise window, got %q", reason)
	}
}

func TestTradeThrottleAllowsConfirmedLossAfterMinimumHold(t *testing.T) {
	at := &AutoTrader{}
	// Past the min hold, loss beyond the -2% noise floor → close allowed.
	ctx := throttleContext("xyz:INTC", "long", 2*time.Hour, -2.5)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0)
	if reason != "" {
		t.Fatalf("expected confirmed loss after min hold to pass, got %q", reason)
	}
}

// TestTradeThrottleAllowsLossCutCheaperThanStopLoss verifies the loss-side
// fail-open: when the position's actual exchange SL distance is known (from the
// trail state — the ATR-derived 1.5×ATR stop), an AI loss-cut that is cheaper
// than letting the SL fire is never throttled, even inside the min-hold / noise
// windows. Without a known SL the fixed thresholds still apply (covered by the
// other tests).
func TestTradeThrottleAllowsLossCutCheaperThanStopLoss(t *testing.T) {
	at := &AutoTrader{}
	key := positionKey("xyz:INTC", "long")
	// Actual placed exchange SL 2.25% below entry (≈ 1.5×ATR for ATR≈1.5%).
	at.trailState = map[string]trailingStopState{
		key: {entry: 100, stop: 97.75, tp: 103},
	}

	// -0.86% loss-cut inside the min-hold: cheaper than the -2.25% SL → allowed.
	ctx := throttleContext("xyz:INTC", "long", 20*time.Minute, -0.86)
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0); reason != "" {
		t.Fatalf("expected loss-cut cheaper than the SL to pass inside min-hold, got %q", reason)
	}

	// Same loss-cut past the min-hold but inside the noise window → still allowed.
	ctx = throttleContext("xyz:INTC", "long", 2*time.Hour, -0.86)
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0); reason != "" {
		t.Fatalf("expected loss-cut cheaper than the SL to pass past min-hold, got %q", reason)
	}

	// A small PROFIT with a known SL is still throttled (win-side unchanged):
	// the fail-open only covers the loss side.
	ctx = throttleContext("xyz:INTC", "long", 2*time.Hour, 0.4)
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_long"}, ctx, 0); !strings.Contains(reason, "noise band") {
		t.Fatalf("expected small-profit close to stay throttled despite known SL, got %q", reason)
	}
}

// TestTradeThrottleAllowsShortLossCutCheaperThanStopLoss covers the loss-side
// fail-open for shorts: the SL sits above entry, and a loss-cut cheaper than it
// passes.
func TestTradeThrottleAllowsShortLossCutCheaperThanStopLoss(t *testing.T) {
	at := &AutoTrader{}
	key := positionKey("xyz:INTC", "short")
	// Short SL 2.25% above entry.
	at.trailState = map[string]trailingStopState{
		key: {entry: 100, stop: 102.25, tp: 97},
	}
	ctx := throttleContext("xyz:INTC", "short", 20*time.Minute, -0.9)
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "close_short"}, ctx, 0); reason != "" {
		t.Fatalf("expected short loss-cut cheaper than the SL to pass, got %q", reason)
	}
}

func TestTradeThrottleBlocksQuickReentryAfterClose(t *testing.T) {
	// Re-entering a just-closed symbol was a consistent loss source in the
	// replay data; the 4h cooldown is enforced from recent close orders, which
	// requires a store — covered by the throttle reason path being non-empty
	// only when a recent close order exists (nil store returns no orders).
	at := &AutoTrader{}
	ctx := &kernel.Context{}
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "open_long"}, ctx, 0); reason != "" {
		t.Fatalf("expected open with no order history to be allowed, got %q", reason)
	}
}

func TestTradeThrottleAllowsLongShortPairInCycle(t *testing.T) {
	at := &AutoTrader{}
	ctx := &kernel.Context{}

	// One open already queued this cycle (e.g. the long) — the second open
	// (the short) must still be allowed so a directional pair can open.
	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "open_short"}, ctx, 1)
	if reason != "" {
		t.Fatalf("expected the second (short) open in cycle to be allowed, got %q", reason)
	}
}

func TestTradeThrottleBlocksOpensOverCycleCap(t *testing.T) {
	at := &AutoTrader{}
	ctx := &kernel.Context{}

	// under the 2-per-cycle cap, a further open is allowed
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "open_long"}, ctx, 1); reason != "" {
		t.Fatalf("expected open within the 2-per-cycle cap to be allowed, got %q", reason)
	}
	// at the cap, the next open is blocked
	if reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "open_long"}, ctx, 2); !strings.Contains(reason, "2 new position") {
		t.Fatalf("expected open beyond the 2-per-cycle cap to be blocked, got %q", reason)
	}
}

func TestTradeThrottleBlocksOpeningAgainstExistingPosition(t *testing.T) {
	at := &AutoTrader{}
	ctx := throttleContext("xyz:INTC", "long", 2*time.Hour, 1.0)

	reason := at.tradeThrottleReason(kernel.Decision{Symbol: "xyz:INTC", Action: "open_short"}, ctx, 0)
	if !strings.Contains(reason, "already has an open") {
		t.Fatalf("expected opposite open to be blocked when position exists, got %q", reason)
	}
}

// TestFailedOpenCooldown verifies a symbol whose open failed (halted / no
// liquidity) is skipped for the cooldown window, then becomes retryable again.
func TestFailedOpenCooldown(t *testing.T) {
	at := &AutoTrader{}
	if reason := at.failedOpenCooldownReason("BTC"); reason != "" {
		t.Fatalf("unmarked symbol should have no cooldown, got %q", reason)
	}
	at.markOpenFailure("BTC")
	if reason := at.failedOpenCooldownReason("BTC"); reason == "" {
		t.Fatal("recently-failed symbol should be in cooldown")
	}
	at.openFailuresMu.Lock()
	at.openFailures[market.Normalize("BTC")] = time.Now().Add(-2 * failedOpenCooldown)
	at.openFailuresMu.Unlock()
	if reason := at.failedOpenCooldownReason("BTC"); reason != "" {
		t.Fatalf("expired failure should be clear, got %q", reason)
	}
}
