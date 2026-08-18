package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"nofx/store"

	"github.com/gin-gonic/gin"
)

// Launch readiness minimums. These are the single source of truth — the
// frontend reads them from the preflight response instead of hardcoding.
const (
	// MinTradingUSDC is the minimum available balance the exchange account
	// needs before the trader can place its first order.
	MinTradingUSDC = 12.0
)

const (
	launchCheckStatusOK      = "ok"
	launchCheckStatusFailed  = "failed"
	launchCheckStatusWarning = "warning"
	launchCheckStatusSkipped = "skipped"
)

// Check IDs — stable identifiers the frontend maps to guided-setup steps.
const (
	launchCheckAIModel         = "ai_model"
	launchCheckStrategy        = "strategy"
	launchCheckExchangeConfig  = "exchange_config"
	launchCheckExchangeAccount = "exchange_account"
	launchCheckExchangeFunds   = "exchange_funds"
)

type LaunchCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Numeric context so the UI can render progress like "8.40 / 12 USDC".
	Required float64  `json:"required,omitempty"`
	Actual   *float64 `json:"actual,omitempty"`
	Asset    string   `json:"asset,omitempty"`
	// Address is the funding address for balance checks, so the UI can offer
	// a deposit shortcut next to the failing item.
	Address string `json:"address,omitempty"`
}

type LaunchPreflightResult struct {
	Ready          bool          `json:"ready"`
	Checks         []LaunchCheck `json:"checks"`
	MinTradingUSDC float64       `json:"min_trading_usdc"`
	CheckedAt      time.Time     `json:"checked_at"`
}

func (r LaunchPreflightResult) failedChecks() []LaunchCheck {
	var failed []LaunchCheck
	for _, check := range r.Checks {
		if check.Status == launchCheckStatusFailed {
			failed = append(failed, check)
		}
	}
	return failed
}

// Summary joins the failing messages into one human-readable sentence.
func (r LaunchPreflightResult) Summary() string {
	failed := r.failedChecks()
	if len(failed) == 0 {
		return ""
	}
	messages := make([]string, 0, len(failed))
	for _, check := range failed {
		if check.Message != "" {
			messages = append(messages, check.Message)
		}
	}
	return strings.Join(messages, " ")
}

type launchPreflightRequest struct {
	AIModelID  string `json:"ai_model_id"`
	ExchangeID string `json:"exchange_id"`
	StrategyID string `json:"strategy_id"`
}

// handleLaunchPreflight runs launch readiness checks for a model/exchange
// pair before any trader is created or mutated. POST /api/launch/preflight
func (s *Server) handleLaunchPreflight(c *gin.Context) {
	userID := c.GetString("user_id")

	var req launchPreflightRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid preflight request")
		return
	}
	if strings.TrimSpace(req.AIModelID) == "" || strings.TrimSpace(req.ExchangeID) == "" {
		SafeBadRequest(c, "ai_model_id and exchange_id are required")
		return
	}

	model, err := s.store.AIModel().Get(userID, req.AIModelID)
	if err != nil {
		model = nil
	}
	exchange, err := s.store.Exchange().GetByID(userID, req.ExchangeID)
	if err != nil {
		exchange = nil
	}

	var strategy *store.Strategy
	strategyRequired := strings.TrimSpace(req.StrategyID) != ""
	if strategyRequired {
		strategy, err = s.store.Strategy().Get(userID, req.StrategyID)
		if err != nil {
			strategy = nil
		}
	}

	result := s.runLaunchPreflight(userID, model, exchange, strategy, strategyRequired)
	c.JSON(http.StatusOK, result)
}

// handleTraderPreflight runs the same checks against an existing trader's
// full configuration. GET /api/traders/:id/preflight
func (s *Server) handleTraderPreflight(c *gin.Context) {
	userID := c.GetString("user_id")
	traderID := c.Param("id")

	fullCfg, err := s.store.Trader().GetFullConfig(userID, traderID)
	if err != nil || fullCfg == nil || fullCfg.Trader == nil {
		SafeNotFound(c, "Trader")
		return
	}

	result := s.runLaunchPreflight(userID, fullCfg.AIModel, fullCfg.Exchange, fullCfg.Strategy, true)
	c.JSON(http.StatusOK, result)
}

// runLaunchPreflight composes every launch readiness check. A check that
// cannot be evaluated (RPC outage, probe timeout) reports "warning" instead
// of "failed" so an infrastructure hiccup never hard-blocks a launch.
func (s *Server) runLaunchPreflight(
	userID string,
	model *store.AIModel,
	exchange *store.Exchange,
	strategy *store.Strategy,
	strategyRequired bool,
) LaunchPreflightResult {
	checks := []LaunchCheck{checkLaunchAIModel(model)}
	checks = append(checks, checkLaunchStrategy(strategy, strategyRequired))
	checks = append(checks, s.checkLaunchExchange(userID, exchange)...)

	ready := true
	for _, check := range checks {
		if check.Status == launchCheckStatusFailed {
			ready = false
			break
		}
	}

	return LaunchPreflightResult{
		Ready:          ready,
		Checks:         checks,
		MinTradingUSDC: MinTradingUSDC,
		CheckedAt:      time.Now().UTC(),
	}
}

func checkLaunchAIModel(model *store.AIModel) LaunchCheck {
	check := LaunchCheck{ID: launchCheckAIModel}

	switch {
	case model == nil:
		check.Status = launchCheckStatusFailed
		check.Code = "MODEL_NOT_FOUND"
		check.Message = "The selected AI model was not found. Configure an AI model first."
	case !model.Enabled:
		check.Status = launchCheckStatusFailed
		check.Code = "MODEL_DISABLED"
		check.Message = fmt.Sprintf("AI model \"%s\" is disabled. Enable it first.", model.Name)
	case strings.TrimSpace(model.APIKey.String()) == "":
		check.Status = launchCheckStatusFailed
		check.Code = "MODEL_MISSING_CREDENTIALS"
		check.Message = fmt.Sprintf("AI model \"%s\" has no credential saved. Add the API key first.", model.Name)
	default:
		check.Status = launchCheckStatusOK
		check.Message = model.Name
	}

	return check
}

func checkLaunchStrategy(strategy *store.Strategy, required bool) LaunchCheck {
	check := LaunchCheck{ID: launchCheckStrategy}

	switch {
	case strategy != nil:
		check.Status = launchCheckStatusOK
		check.Message = strategy.Name
	case required:
		check.Status = launchCheckStatusFailed
		check.Code = "STRATEGY_NOT_FOUND"
		check.Message = "The selected strategy was not found. Pick or create a strategy first."
	default:
		check.Status = launchCheckStatusSkipped
	}

	return check
}

// checkLaunchExchange validates exchange configuration completeness and then
// probes the live account (30s server cache) for status and balance.
func (s *Server) checkLaunchExchange(userID string, exchange *store.Exchange) []LaunchCheck {
	configCheck := LaunchCheck{ID: launchCheckExchangeConfig}
	accountCheck := LaunchCheck{ID: launchCheckExchangeAccount}
	fundsCheck := LaunchCheck{ID: launchCheckExchangeFunds, Required: MinTradingUSDC}

	if msg, code := describeExchangeConfigIssue(exchange); code != "" {
		configCheck.Status = launchCheckStatusFailed
		configCheck.Code = code
		configCheck.Message = msg
		accountCheck.Status = launchCheckStatusSkipped
		fundsCheck.Status = launchCheckStatusSkipped
		return []LaunchCheck{configCheck, accountCheck, fundsCheck}
	}

	configCheck.Status = launchCheckStatusOK
	configCheck.Message = exchangeDisplayName(exchange)
	fundsCheck.Asset = accountAssetForExchange(exchange.ExchangeType)

	states, err := s.getExchangeAccountStates(userID)
	if err != nil {
		accountCheck.Status = launchCheckStatusWarning
		accountCheck.Code = "EXCHANGE_STATE_UNKNOWN"
		accountCheck.Message = "Could not verify the exchange account right now."
		fundsCheck.Status = launchCheckStatusSkipped
		return []LaunchCheck{configCheck, accountCheck, fundsCheck}
	}

	state, ok := states[exchange.ID]
	if !ok {
		accountCheck.Status = launchCheckStatusWarning
		accountCheck.Code = "EXCHANGE_STATE_UNKNOWN"
		accountCheck.Message = "Could not verify the exchange account right now."
		fundsCheck.Status = launchCheckStatusSkipped
		return []LaunchCheck{configCheck, accountCheck, fundsCheck}
	}

	if state.Status != exchangeAccountStatusOK {
		accountCheck.Status = launchCheckStatusFailed
		accountCheck.Code = state.ErrorCode
		accountCheck.Message = state.ErrorMessage
		if accountCheck.Message == "" {
			accountCheck.Message = fmt.Sprintf("Exchange account \"%s\" is not ready (%s).", exchangeDisplayName(exchange), state.Status)
		}
		fundsCheck.Status = launchCheckStatusSkipped
		return []LaunchCheck{configCheck, accountCheck, fundsCheck}
	}

	accountCheck.Status = launchCheckStatusOK
	accountCheck.Message = state.DisplayBalance

	// Gate on the better of available balance and total equity: a bot with
	// capital deployed in open positions has low *available* margin but is
	// clearly funded — restarting it must not be blocked.
	funded := state.AvailableBalance
	if state.TotalEquity > funded {
		funded = state.TotalEquity
	}
	fundsCheck.Actual = &funded

	if funded < MinTradingUSDC {
		message := fmt.Sprintf(
			"Exchange account \"%s\" holds %.2f %s but needs at least %.0f %s to place the first trade.",
			exchangeDisplayName(exchange), funded, fundsCheck.Asset, MinTradingUSDC, fundsCheck.Asset,
		)
		if exchange.Testnet {
			// Testnet balances are play money — warn instead of block.
			fundsCheck.Status = launchCheckStatusWarning
		} else {
			fundsCheck.Status = launchCheckStatusFailed
		}
		fundsCheck.Code = "EXCHANGE_INSUFFICIENT_FUNDS"
		fundsCheck.Message = message
	} else {
		fundsCheck.Status = launchCheckStatusOK
	}

	return []LaunchCheck{configCheck, accountCheck, fundsCheck}
}

// describeExchangeConfigIssue mirrors the create-time exchange validation but
// returns stable uppercase codes for the checklist UI.
func describeExchangeConfigIssue(exchange *store.Exchange) (string, string) {
	if exchange == nil {
		return "The selected exchange account was not found. Connect an exchange first.", "EXCHANGE_NOT_FOUND"
	}
	if !exchange.Enabled {
		return fmt.Sprintf("Exchange account \"%s\" is disabled. Enable it first.", exchangeDisplayName(exchange)), "EXCHANGE_DISABLED"
	}
	if missing := missingExchangeFields(exchange); len(missing) > 0 {
		return fmt.Sprintf(
			"Exchange account \"%s\" is missing %s. Complete the connection first.",
			exchangeDisplayName(exchange), strings.Join(missing, ", "),
		), "EXCHANGE_MISSING_FIELDS"
	}
	if exchange.ExchangeType == "hyperliquid" && !exchange.HyperliquidBuilderApproved {
		return "Hyperliquid builder authorization is not complete. Reconnect the wallet and finish the authorization.", "HYPERLIQUID_BUILDER_NOT_APPROVED"
	}
	return "", ""
}
