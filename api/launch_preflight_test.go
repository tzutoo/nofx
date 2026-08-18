package api

import (
	"testing"

	"nofx/crypto"
	"nofx/store"
)

// Well-known throwaway development key (hardhat account #1) — never funded.
// Used as a stand-in ECDSA hex key for Hyperliquid accounts in tests.
const testKeyHex = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

func findCheck(t *testing.T, checks []LaunchCheck, id string) LaunchCheck {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("check %q not found in %+v", id, checks)
	return LaunchCheck{}
}

func TestCheckLaunchAIModel(t *testing.T) {
	if got := checkLaunchAIModel(nil); got.Status != launchCheckStatusFailed || got.Code != "MODEL_NOT_FOUND" {
		t.Fatalf("nil model: expected failed/MODEL_NOT_FOUND, got %+v", got)
	}

	disabled := &store.AIModel{Name: "DeepSeek", Provider: "deepseek", Enabled: false}
	if got := checkLaunchAIModel(disabled); got.Code != "MODEL_DISABLED" {
		t.Fatalf("disabled model: expected MODEL_DISABLED, got %+v", got)
	}

	noKey := &store.AIModel{Name: "DeepSeek", Provider: "deepseek", Enabled: true}
	if got := checkLaunchAIModel(noKey); got.Code != "MODEL_MISSING_CREDENTIALS" {
		t.Fatalf("missing key: expected MODEL_MISSING_CREDENTIALS, got %+v", got)
	}

	ready := &store.AIModel{Name: "DeepSeek", Provider: "deepseek", Enabled: true, APIKey: crypto.EncryptedString("sk-test")}
	if got := checkLaunchAIModel(ready); got.Status != launchCheckStatusOK {
		t.Fatalf("ready model: expected ok, got %+v", got)
	}
}

func TestDescribeExchangeConfigIssue(t *testing.T) {
	if _, code := describeExchangeConfigIssue(nil); code != "EXCHANGE_NOT_FOUND" {
		t.Fatalf("nil exchange: expected EXCHANGE_NOT_FOUND, got %s", code)
	}

	disabled := &store.Exchange{ID: "ex", ExchangeType: "hyperliquid", Enabled: false}
	if _, code := describeExchangeConfigIssue(disabled); code != "EXCHANGE_DISABLED" {
		t.Fatalf("disabled: expected EXCHANGE_DISABLED, got %s", code)
	}

	missing := &store.Exchange{ID: "ex", ExchangeType: "hyperliquid", Enabled: true}
	if _, code := describeExchangeConfigIssue(missing); code != "EXCHANGE_MISSING_FIELDS" {
		t.Fatalf("missing fields: expected EXCHANGE_MISSING_FIELDS, got %s", code)
	}

	unapproved := &store.Exchange{
		ID:                    "ex",
		ExchangeType:          "hyperliquid",
		Enabled:               true,
		APIKey:                crypto.EncryptedString(testKeyHex),
		HyperliquidWalletAddr: "0x1111111111111111111111111111111111111111",
	}
	if _, code := describeExchangeConfigIssue(unapproved); code != "HYPERLIQUID_BUILDER_NOT_APPROVED" {
		t.Fatalf("builder unapproved: expected HYPERLIQUID_BUILDER_NOT_APPROVED, got %s", code)
	}

	unapproved.HyperliquidBuilderApproved = true
	if _, code := describeExchangeConfigIssue(unapproved); code != "" {
		t.Fatalf("ready exchange: expected no issue, got %s", code)
	}
}

func readyHyperliquidExchange() *store.Exchange {
	return &store.Exchange{
		ID:                         "ex-hl",
		ExchangeType:               "hyperliquid",
		Enabled:                    true,
		APIKey:                     crypto.EncryptedString(testKeyHex),
		HyperliquidWalletAddr:      "0x1111111111111111111111111111111111111111",
		HyperliquidBuilderApproved: true,
	}
}

func preflightTestServer(t *testing.T, userID string, states map[string]ExchangeAccountState) *Server {
	t.Helper()
	server := &Server{exchangeAccountStateCache: NewExchangeAccountStateCache()}
	if states != nil {
		server.exchangeAccountStateCache.Set(userID, states)
	}
	return server
}

func TestCheckLaunchExchangeInsufficientFundsBlocks(t *testing.T) {
	exchange := readyHyperliquidExchange()
	server := preflightTestServer(t, "user-1", map[string]ExchangeAccountState{
		exchange.ID: {ExchangeID: exchange.ID, Status: exchangeAccountStatusOK, AvailableBalance: 5.5, TotalEquity: 5.5},
	})

	checks := server.checkLaunchExchange("user-1", exchange)
	funds := findCheck(t, checks, launchCheckExchangeFunds)
	if funds.Status != launchCheckStatusFailed || funds.Code != "EXCHANGE_INSUFFICIENT_FUNDS" {
		t.Fatalf("expected failed/EXCHANGE_INSUFFICIENT_FUNDS, got %+v", funds)
	}
	if funds.Actual == nil || *funds.Actual != 5.5 {
		t.Fatalf("expected actual 5.5, got %+v", funds.Actual)
	}
}

func TestCheckLaunchExchangeDeployedMarginPasses(t *testing.T) {
	// A running bot with capital locked in positions: low available balance
	// but healthy equity. Restart must not be blocked.
	exchange := readyHyperliquidExchange()
	server := preflightTestServer(t, "user-1", map[string]ExchangeAccountState{
		exchange.ID: {ExchangeID: exchange.ID, Status: exchangeAccountStatusOK, AvailableBalance: 3, TotalEquity: 100},
	})

	checks := server.checkLaunchExchange("user-1", exchange)
	funds := findCheck(t, checks, launchCheckExchangeFunds)
	if funds.Status != launchCheckStatusOK {
		t.Fatalf("deployed margin with healthy equity should pass, got %+v", funds)
	}
	if funds.Actual == nil || *funds.Actual != 100 {
		t.Fatalf("expected actual 100 (equity), got %+v", funds.Actual)
	}
}

func TestCheckLaunchExchangeTestnetLowFundsIsWarning(t *testing.T) {
	exchange := readyHyperliquidExchange()
	exchange.Testnet = true
	server := preflightTestServer(t, "user-1", map[string]ExchangeAccountState{
		exchange.ID: {ExchangeID: exchange.ID, Status: exchangeAccountStatusOK, AvailableBalance: 0},
	})

	checks := server.checkLaunchExchange("user-1", exchange)
	funds := findCheck(t, checks, launchCheckExchangeFunds)
	if funds.Status != launchCheckStatusWarning {
		t.Fatalf("testnet low funds should warn, not block, got %+v", funds)
	}
}

func TestCheckLaunchExchangeInvalidCredentials(t *testing.T) {
	exchange := readyHyperliquidExchange()
	server := preflightTestServer(t, "user-1", map[string]ExchangeAccountState{
		exchange.ID: {
			ExchangeID:   exchange.ID,
			Status:       exchangeAccountStatusInvalidCredentials,
			ErrorCode:    "INVALID_CREDENTIALS",
			ErrorMessage: "Exchange credentials are invalid",
		},
	})

	checks := server.checkLaunchExchange("user-1", exchange)
	account := findCheck(t, checks, launchCheckExchangeAccount)
	if account.Status != launchCheckStatusFailed || account.Code != "INVALID_CREDENTIALS" {
		t.Fatalf("expected failed/INVALID_CREDENTIALS, got %+v", account)
	}
	if funds := findCheck(t, checks, launchCheckExchangeFunds); funds.Status != launchCheckStatusSkipped {
		t.Fatalf("funds check should be skipped when account probe fails, got %+v", funds)
	}
}

func TestRunLaunchPreflightAggregatesReadiness(t *testing.T) {
	exchange := readyHyperliquidExchange()
	server := preflightTestServer(t, "user-1", map[string]ExchangeAccountState{
		exchange.ID: {ExchangeID: exchange.ID, Status: exchangeAccountStatusOK, AvailableBalance: 100, TotalEquity: 100},
	})
	model := &store.AIModel{Name: "DeepSeek", Provider: "deepseek", Enabled: true, APIKey: crypto.EncryptedString("sk-test")}
	strategy := &store.Strategy{ID: "strat-1", Name: "Autopilot"}

	result := server.runLaunchPreflight("user-1", model, exchange, strategy, true)
	if !result.Ready {
		t.Fatalf("expected ready, got %+v", result)
	}
	if result.MinTradingUSDC != MinTradingUSDC {
		t.Fatalf("minimum trading balance must be exposed in the response, got %+v", result)
	}

	// Break one prerequisite → not ready, and Summary explains it.
	broken := &store.AIModel{Name: "DeepSeek", Provider: "deepseek", Enabled: true}
	result = server.runLaunchPreflight("user-1", broken, exchange, strategy, true)
	if result.Ready {
		t.Fatalf("expected not ready with missing model credential, got %+v", result)
	}
	if result.Summary() == "" {
		t.Fatalf("summary should describe the failing check")
	}
}

func TestRunLaunchPreflightHealthySetupPasses(t *testing.T) {
	exchange := readyHyperliquidExchange()
	server := preflightTestServer(t, "user-1", map[string]ExchangeAccountState{
		exchange.ID: {ExchangeID: exchange.ID, Status: exchangeAccountStatusOK, AvailableBalance: 100},
	})
	model := &store.AIModel{Name: "DeepSeek", Provider: "deepseek", Enabled: true, APIKey: crypto.EncryptedString("sk-test")}

	result := server.runLaunchPreflight("user-1", model, exchange, nil, false)
	if !result.Ready {
		t.Fatalf("healthy setup must not block launch, got %+v", result)
	}
}
