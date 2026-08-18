package trader

// Runtime health state written by the run-loop goroutine and read by the API
// layer (GetStatus). Everything here goes through runtimeHealthMu so the
// dashboard can poll without racing the trading loop.

func (at *AutoTrader) setSafeMode(active bool, reason string) {
	at.runtimeHealthMu.Lock()
	at.safeMode = active
	at.safeModeReason = reason
	at.runtimeHealthMu.Unlock()
}

func (at *AutoTrader) isSafeMode() bool {
	at.runtimeHealthMu.RLock()
	defer at.runtimeHealthMu.RUnlock()
	return at.safeMode
}

func (at *AutoTrader) safeModeState() (bool, string) {
	at.runtimeHealthMu.RLock()
	defer at.runtimeHealthMu.RUnlock()
	return at.safeMode, at.safeModeReason
}
