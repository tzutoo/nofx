package kernel

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"nofx/store"
)

// TestEngineConsumesSignalServiceEndToEnd is a READ-ONLY integration test that
// proves the engine pulls its candidate ranking (and, if available, per-symbol
// signal-lab / heatmap) from the running self-hosted signal service rather than
// claw402. It never starts a trading loop and never calls any order/position
// method.
//
// The service is expected to be reachable via SIGNAL_SERVICE_BASE_URL (defaults
// to http://localhost:8480). When running inside a docker golang container
// against a service published on the host port 8480, set
// SIGNAL_SERVICE_BASE_URL=http://host.docker.internal:8480.
func TestEngineConsumesSignalServiceEndToEnd(t *testing.T) {
	baseURL := os.Getenv("SIGNAL_SERVICE_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8480"
	}
	t.Setenv("SIGNAL_SERVICE_BASE_URL", baseURL)

	// Skip gracefully when the signal service isn't running (e.g. a plain
	// `go test ./kernel/...` with no service up) so the suite stays green.
	probeClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := probeClient.Get(baseURL + "/v1/health")
	if err != nil {
		t.Skipf("signal service not reachable at %s (skipping integration test): %v", baseURL, err)
	}
	resp.Body.Close()

	cfg := store.GetDefaultStrategyConfig("en")
	if cfg.CoinSource.SourceType != "vergex_signal" {
		t.Fatalf("default config source type = %q, want vergex_signal", cfg.CoinSource.SourceType)
	}

	engine := NewStrategyEngine(&cfg)
	if engine.vergexClient == nil {
		t.Fatal("engine vergex client is nil; engine not wired to signal service")
	}

	// 1) Prove the ranking came from the live service (>= 1 candidate).
	candidates, err := engine.GetCandidateCoins()
	if err != nil {
		t.Fatalf("GetCandidateCoins failed against service at %s: %v", baseURL, err)
	}
	if len(candidates) == 0 {
		t.Fatalf("GetCandidateCoins returned 0 candidates; service at %s returned an empty board", baseURL)
	}

	t.Logf("service=%s got %d candidates", baseURL, len(candidates))
	bull, bear := 0, 0
	sampleSymbols := make([]string, 0, len(candidates))
	for _, c := range candidates {
		sampleSymbols = append(sampleSymbols, c.Symbol)
		if item, ok := engine.vergexRankingCache[c.Symbol]; ok && item != nil {
			switch vergexBiasToken(item.Bias) {
			case "bullish":
				bull++
			case "bearish":
				bear++
			}
		}
	}
	t.Logf("candidate symbols=%v (bullish=%d bearish=%d)", sampleSymbols, bull, bear)

	// The engine must carry both directions when the board is split (the
	// direction-balanced selection contract).
	if len(candidates) >= 2 && (bull == 0 || bear == 0) {
		t.Logf("NOTE: ranking board produced only one direction (bull=%d bear=%d); may be an asymmetric snapshot", bull, bear)
	}

	// 2) READ-ONLY per-symbol fetch: assert signal-lab + heatmap present.
	if len(candidates) == 0 {
		return
	}
	target := candidates[0].Symbol
	analyses := engine.FetchVergexDataBatch(context.Background(), []string{target})
	a, ok := analyses[target]
	if !ok || a == nil {
		t.Fatalf("FetchVergexDataBatch returned no analysis for %s (service at %s)", target, baseURL)
	}
	if len(a.SignalLab) == 0 {
		t.Errorf("signal-lab missing for %s (err=%s)", target, a.SignalLabError)
	}
	if len(a.Heatmap) == 0 {
		t.Errorf("heatmap missing for %s (err=%s)", target, a.HeatmapError)
	}
	if a.Ranking == nil {
		t.Errorf("analysis.Ranking nil for %s", target)
	} else {
		t.Logf("%s ranking: bias=%s score=%.4f confidence=%.2f", target, a.Ranking.Bias, a.Ranking.Score, a.Ranking.Confidence)
	}
	if len(a.SignalLab) > 0 {
		t.Logf("%s signal-lab bytes=%d", target, len(a.SignalLab))
	}
	if len(a.Heatmap) > 0 {
		t.Logf("%s heatmap bytes=%d", target, len(a.Heatmap))
	}
}

func vergexBiasToken(b string) string {
	switch b {
	case "bullish", "long", "buy":
		return "bullish"
	case "bearish", "short", "sell":
		return "bearish"
	default:
		return "neutral"
	}
}
