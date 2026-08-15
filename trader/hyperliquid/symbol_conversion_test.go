package hyperliquid

import "testing"

func TestConvertSymbolToHyperliquidXYZAliases(t *testing.T) {
	cases := map[string]string{
		"SAMSUNG-USDC":  "xyz:SMSN",
		"SK-HYNIX-USDC": "xyz:SKHX",
		"TSLAUSDT":      "xyz:TSLA",
		"xyz:SMSN":      "xyz:SMSN",
		"HYPEUSDT":      "HYPE",
	}
	for input, want := range cases {
		if got := convertSymbolToHyperliquid(input); got != want {
			t.Fatalf("convertSymbolToHyperliquid(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLeverageCoinStripsXYZPrefix(t *testing.T) {
	cases := map[string]string{
		"xyz:CL":       "CL",
		"xyz:USAR":     "USAR",
		"xyz:TSLA":     "TSLA",
		"HYPEUSDT":     "HYPE", // core perp, no prefix to strip
		"SAMSUNG-USDC": "SMSN",
	}
	for input, want := range cases {
		if got := leverageCoin(input); got != want {
			t.Fatalf("leverageCoin(%q) = %q, want %q", input, got, want)
		}
	}
}
