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

// pineifyCryptoTicker resolves a Hyperliquid crypto-major base to the bare
// Pineify ticker it maps to (e.g. BTC -> "BTC", enriched as the Binance USDT
// pair BTCUSDT), or "" if the base is not a Pineify-covered crypto major.
//
// This is the crypto path, distinct from pineifyTicker's US-stock path. Crypto
// majors are enriched with TA ONLY (get-technical-analysis-snapshot on the
// bare <BASE>USDT ticker); events and rating are US-equity only and Pineify
// returns nothing for them. Only real Binance USDT pairs AND plausible
// Hyperliquid core_perp names are included, so long-tail / exotic names that
// either don't trade on Binance or differ from HL's naming are excluded.
func pineifyCryptoTicker(base string) string {
	base = strings.ToUpper(strings.TrimSpace(base))
	base = strings.TrimPrefix(base, "XYZ:")
	if base == "" {
		return ""
	}
	if !pineifyCryptoMajors[base] {
		return ""
	}
	return base
}

// pineifyCryptoMajors is the set of Hyperliquid core_perp crypto majors that
// also trade on Binance USDT (so Pineify's TA tool, which uses bare
// <BASE>USDT tickers, can resolve them). BTC/ETH and the top-cap alts cover the
// ~20 majors that map cleanly from HL core_perp naming to Binance.
var pineifyCryptoMajors = map[string]bool{
	"BTC":  true,
	"ETH":  true,
	"SOL":  true,
	"XRP":  true,
	"DOGE": true,
	"ADA":  true,
	"AVAX": true,
	"BNB":  true,
	"LINK": true,
	"LTC":  true,
	"BCH":  true,
	"ATOM": true,
	"XMR":  true,
	"NEAR": true,
	"SUI":  true,
	"APT":  true,
	"INJ":  true,
	"DOT":  true,
	"UNI":  true,
	"TRX":  true,
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
	"JP225":  true, // Japan-225 index
	"KR200":  true, // Korea-200 index
	"DXY":    true, // Dollar index
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
