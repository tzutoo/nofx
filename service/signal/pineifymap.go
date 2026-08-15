package signal

import (
	"strings"

	"nofx/provider/hyperliquid"
)

// pineifyTicker resolves a Hyperliquid base symbol to the bare Pineify ticker
// it maps to, or "" if the symbol has no Pineify coverage.
//
// Mappability is derived from the already-computed `Category` (coins.go
// XYZCategory) — the classifier that actually feeds products — NOT from the
// UI-only classifiers in handler_klines.go. Only the US-listed `stock` subset
// maps. Indexes, commodities, forex, pre-IPO and crypto do not map (Pineify's
// screener/rating/technical tools are US-equity centric). A small exclusion
// set drops stock-classified but non-US / index / ETF issues so we don't waste
// a rated call on a ticker Pineify cannot resolve.
func pineifyTicker(base string) string {
	base = strings.ToUpper(strings.TrimSpace(base))
	base = strings.TrimPrefix(base, "XYZ:")
	if base == "" {
		return ""
	}
	if hyperliquid.XYZCategory(base) != "stock" {
		return ""
	}
	if pineifyNonMappable[base] {
		return ""
	}
	return base
}

// pineifyNonMappable is the hardened exclusion set for stock-classified but
// non-US issuers, indexes and ETFs. Pineify is US-listed equities; these either
// are not listed US or resolve poorly (returns none) on Pineify screeners.
var pineifyNonMappable = map[string]bool{
	// Non-US issuers classified as stock by coins.go XYZCategory.
	"SMSN": true, // Samsung
	"SKHX": true, // SK Hynix
	"SKHY": true, // SK Hynix (alternate ticker)
	"LVMH": true,
	"SONY": true,
	"TM":   true,
	"RACE": true,
	"VOW3": true,
	"BMW":  true,
	"MBG":  true,
	"BABA": true, // ADR-listed HK primary — leave as non-mappable fallback
	"TSM":  true,
	// Index / ETF / benchmark issues that can slip into the stock bucket.
	"SP500":  true,
	"SPX":    true,
	"NDX":    true,
	"DJI":    true,
	"VIX":    true,
	"XYZ100": true,
	"XYZ25":  true,
	"XYZ50":  true,
	"DAX":    true,
	"FTSE":   true,
	"NIKKEI": true,
	"HSI":    true,
	"CSI300": true,
	"XLE":    true,
	"EWY":    true,
	"EWJ":    true,
	"EWZ":    true,
	"EWT":    true,
	"NIFTY":  true,
	"IBOV":   true,
	"GOLD":   true,
	"SILVER": true,
	"CL":     true,
	"SOXL":   true, // 3x ETF
	"SPCX":   true, // pre-IPO SpaceX
}
