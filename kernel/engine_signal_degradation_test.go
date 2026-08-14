package kernel

import (
	"strings"
	"testing"

	"nofx/store"
)

// TestGetCandidateCoinsErrorsWhenSignalServiceUnreachable proves the "service
// down" path surfaces as an error: NewStrategyEngine always builds a vergex
// client at SIGNAL_SERVICE_BASE_URL, so pointing it at an unreachable address
// must make GetCandidateCoins (vergex_signal) return a non-nil error with no
// candidates, rather than silently succeeding or panicking.
//
// The trading loop treats this as a graceful skip, not a fatal abort:
//   - auto_trader_loop.go buildTradingContext (~line 592): on GetCandidateCoins
//     error it logs "Failed to get candidate coins" and keeps the candidate
//     list empty — it never returns the error up the stack.
//   - auto_trader_loop.go runCycle (~line 85): a nil/empty CandidateCoins list
//     hits the "No candidate coins available, cycle skipped" branch, which
//     records Success=true and returns nil (no abort, no panic).
func TestGetCandidateCoinsErrorsWhenSignalServiceUnreachable(t *testing.T) {
	// 127.0.0.1:1 is almost certainly not listening -> connection refused.
	t.Setenv("SIGNAL_SERVICE_BASE_URL", "http://127.0.0.1:1")

	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "vergex_signal"
	cfg.CoinSource.VergexLimit = 5

	engine := NewStrategyEngine(&cfg)
	if engine.vergexClient == nil {
		t.Fatal("engine vergex client is nil; engine not wired to signal service")
	}

	candidates, err := engine.GetCandidateCoins()
	if err == nil {
		t.Fatalf("expected GetCandidateCoins to error with unreachable service, got %d candidates", len(candidates))
	}
	if len(candidates) != 0 {
		t.Fatalf("expected zero candidates on service-down error, got %d", len(candidates))
	}

	// The error should clearly identify the downed data source so operators
	// (and the loop's warning log) can tell a service failure from a market
	// signal condition.
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "vergex") && !strings.Contains(lower, "failed") {
		t.Errorf("error should reference the failing signal source, got: %v", err)
	}
}

// TestCandidateCoinsErrorMeansEmptyCandidatesForLoop documents the contract the
// trading loop relies on: a GetCandidateCoins error maps to an empty candidate
// list (the runCycle "cycle skipped" path), never to a raised error that would
// abort the whole cycle. We assert the engine-side half of that contract
// (error + empty candidates) directly; the loop's empty-list skip branch is
// the documented consumer in auto_trader_loop.go.
func TestCandidateCoinsErrorMeansEmptyCandidatesForLoop(t *testing.T) {
	t.Setenv("SIGNAL_SERVICE_BASE_URL", "http://127.0.0.1:1")
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "vergex_signal"
	engine := NewStrategyEngine(&cfg)
	candidates, err := engine.GetCandidateCoins()
	if err == nil {
		t.Skip("service unexpectedly reachable; loop-skip contract not exercised")
	}
	if len(candidates) != 0 {
		t.Fatalf("error must yield an empty candidate list for the loop's skip branch, got %d", len(candidates))
	}
}
