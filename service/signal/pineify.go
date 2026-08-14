package signal

// Pineify augmentation is optional and deferred (work item 7). This file holds
// the seam only: when PINEIFY_MCP_TOKEN is set, the ingest worker may enrich
// xyz:/crypto-major signals with Pineify technicals/ratings/flow. It is never a
// hard dependency and is rate-limited to Config.PineifyRatePerMinute in the
// ingest worker only.
//
// Note: all Pineify calls must run inside the single ingest worker (batched
// per refresh), never on the HTTP request path or the trading loop, because
// Pineify rate-limits to a few calls per minute.

// pineifyEnabled reports whether Pineify augmentation is configured.
func (s *Service) pineifyEnabled() bool {
	return s.cfg != nil && s.cfg.PineifyMCPToken != ""
}
