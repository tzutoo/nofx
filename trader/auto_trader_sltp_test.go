package trader

import (
	"testing"

	"nofx/kernel"
)

func TestSLTPDefaultsLong(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_long", Symbol: "BTCUSDT"}
	if err := at.ensureStopLossTakeProfitDefaults(d, 100, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.StopLoss != 97 || d.TakeProfit != 108 {
		t.Fatalf("long defaults wrong: SL=%.2f TP=%.2f, want 97/108", d.StopLoss, d.TakeProfit)
	}
}

func TestSLTPDefaultsShort(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_short", Symbol: "BTCUSDT"}
	if err := at.ensureStopLossTakeProfitDefaults(d, 100, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.StopLoss != 103 || d.TakeProfit != 92 {
		t.Fatalf("short defaults wrong: SL=%.2f TP=%.2f, want 103/92", d.StopLoss, d.TakeProfit)
	}
}

func TestSLTPPartialFillFillsBoth(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_long", Symbol: "BTCUSDT", StopLoss: 95}
	if err := at.ensureStopLossTakeProfitDefaults(d, 100, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Either missing -> fill both from entry (not preserve the provided SL).
	if d.StopLoss != 97 || d.TakeProfit != 108 {
		t.Fatalf("partial fill wrong: SL=%.2f TP=%.2f, want 97/108", d.StopLoss, d.TakeProfit)
	}
}

func TestSLTPNoOverrideWhenBothSet(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_long", Symbol: "BTCUSDT", StopLoss: 95, TakeProfit: 110}
	if err := at.ensureStopLossTakeProfitDefaults(d, 100, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.StopLoss != 95 || d.TakeProfit != 110 {
		t.Fatalf("valid pair overridden: SL=%.2f TP=%.2f", d.StopLoss, d.TakeProfit)
	}
}

func TestSLTPEntryZeroErrors(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_long", Symbol: "BTCUSDT"}
	if err := at.ensureStopLossTakeProfitDefaults(d, 0, 0); err == nil {
		t.Fatal("expected error for invalid entry price")
	}
	if d.StopLoss != 0 || d.TakeProfit != 0 {
		t.Fatalf("decision mutated on error: SL=%.2f TP=%.2f", d.StopLoss, d.TakeProfit)
	}
}

func TestSLTPDefaultsATR(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_long", Symbol: "BTCUSDT"}
	// atr14=2, entry=100 -> stop = 100 - 1.5*2 = 97, target = 100 + 2*2 = 104.
	if err := at.ensureStopLossTakeProfitDefaults(d, 100, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.StopLoss != 97 || d.TakeProfit != 104 {
		t.Fatalf("ATR long defaults wrong: SL=%.2f TP=%.2f, want 97/104", d.StopLoss, d.TakeProfit)
	}
}

func TestSLTPDefaultsATRShort(t *testing.T) {
	at := &AutoTrader{}
	d := &kernel.Decision{Action: "open_short", Symbol: "BTCUSDT"}
	// stop = 100 + 1.5*2 = 103, target = 100 - 2*2 = 96.
	if err := at.ensureStopLossTakeProfitDefaults(d, 100, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.StopLoss != 103 || d.TakeProfit != 96 {
		t.Fatalf("ATR short defaults wrong: SL=%.2f TP=%.2f, want 103/96", d.StopLoss, d.TakeProfit)
	}
}
