package api

import (
	"context"
	"fmt"
	"net/http"
	"nofx/logger"
	"nofx/provider/vergex"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleVergexSignalRanking(c *gin.Context) {
	client, ok := s.newVergexClientForRequest(c)
	if !ok {
		return
	}
	data, err := client.GetSignalRanking(context.Background(), vergex.Query{
		Chain:   strings.TrimSpace(c.Query("chain")),
		LiqBand: strings.TrimSpace(c.Query("liqBand")),
	})
	if err != nil {
		logger.Warnf("Vergex signal-ranking failed: %v", err)
		c.JSON(vergexErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	limit := parsePositiveInt(c.Query("limit"), vergex.MaxSignalRankingItems)
	marketType := strings.TrimSpace(c.Query("marketType"))
	items := vergex.FilterSignalRankingItems(data.Items, marketType, limit)
	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"raw":   data.Raw,
	})
}

func (s *Server) handleVergexSignalLab(c *gin.Context) {
	client, ok := s.newVergexClientForRequest(c)
	if !ok {
		return
	}
	body, err := client.GetSignalLab(context.Background(), vergex.Query{
		MarketType: withDefault(strings.TrimSpace(c.Query("marketType")), vergex.DefaultMarketType),
		Symbol:     strings.TrimSpace(c.Query("symbol")),
		Chain:      strings.TrimSpace(c.Query("chain")),
		LiqBand:    strings.TrimSpace(c.Query("liqBand")),
	})
	if err != nil {
		logger.Warnf("Vergex signal-lab failed: %v", err)
		c.JSON(vergexErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (s *Server) handleVergexCostLiquidationHeatmap(c *gin.Context) {
	client, ok := s.newVergexClientForRequest(c)
	if !ok {
		return
	}
	body, err := client.GetCostLiquidationHeatmap(context.Background(), vergex.Query{
		MarketType: withDefault(strings.TrimSpace(c.Query("marketType")), vergex.DefaultMarketType),
		Symbol:     strings.TrimSpace(c.Query("symbol")),
		Chain:      strings.TrimSpace(c.Query("chain")),
		LiqBand:    strings.TrimSpace(c.Query("liqBand")),
	})
	if err != nil {
		logger.Warnf("Vergex cost-liquidation-heatmap failed: %v", err)
		c.JSON(vergexErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// handleVergexFlowMarkets proxies the Vergex net-flow market ranking from the
// self-hosted signal service (no wallet key). The upstream JSON is passed
// through verbatim: { data: { window, by, inflow: [{ symbol, netFlow,
// buyNotional, sellNotional, trades, latestPrice }, ...] } }.
func (s *Server) handleVergexFlowMarkets(c *gin.Context) {
	client, ok := s.newVergexClientForRequest(c)
	if !ok {
		return
	}
	chain := withDefault(strings.TrimSpace(c.Query("chain")), "mainnet")
	window := withDefault(strings.TrimSpace(c.Query("window")), "1h")
	limit := parsePositiveInt(c.Query("limit"), 25)

	body, err := client.GetFlowMarkets(context.Background(), chain, window, limit)
	if err != nil {
		logger.Warnf("Vergex flow-markets failed: %v", err)
		c.JSON(vergexErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func (s *Server) newVergexClientForRequest(c *gin.Context) (*vergex.Client, bool) {
	// Require an authenticated user (the endpoints are in the protected group).
	if c.GetString("user_id") == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil, false
	}
	// The self-hosted signal service requires no wallet key.
	return vergex.NewClient("", "", &logger.MCPLogger{}), true
}

func parsePositiveInt(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

func withDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// vergexErrorStatus maps a vergex client error back to an HTTP status code.
// The client wraps non-200 responses as "vergex HTTP <status> (<path>): body",
// so a 404 "market not found" stays a 404 and a 503 degraded service stays a
// 503 instead of every error collapsing to 502.
func vergexErrorStatus(err error) int {
	s := err.Error()
	for _, code := range []int{400, 401, 403, 404, 409, 422, 429, 500, 502, 503} {
		if strings.Contains(s, fmt.Sprintf("vergex HTTP %d ", code)) {
			return code
		}
	}
	return http.StatusBadGateway
}
