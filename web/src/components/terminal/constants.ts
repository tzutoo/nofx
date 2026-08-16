// Refresh cadence (ms) for the vergex terminal panels (Signal Matrix, Flow,
// Liquidation Map). These are served by the FREE self-hosted signal service
// (SIGNAL_SERVICE_BASE_URL, default :8480) and re-ranked each
// SIGNAL_SERVICE_INTERVAL cycle (default 3m). Tune alongside the service
// interval so polls never outpace a fresh snapshot.
export const VERGEX_TERMINAL_REFRESH_MS = 180_000
