package signal

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// NewHTTPHandler wires the service's HTTP routes.
func NewHTTPHandler(s *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/products", s.handleProducts)
	mux.HandleFunc("/v1/signal/ranking", s.handleRanking)
	mux.HandleFunc("/v1/signal/lab", s.handleSignalLab)
	mux.HandleFunc("/v1/signal/heatmap", s.handleHeatmap)
	mux.HandleFunc("/v1/netflow/ranking", s.handleNetFlow)
	mux.HandleFunc("/v1/signal/priority", s.handlePriority)
	return mux
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	_, _, last, _ := s.Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":          true,
		"last_ingest": last.UTC().Format(time.RFC3339),
		"age_ms":      time.Since(last).Milliseconds(),
	})
}

func (s *Service) handleProducts(w http.ResponseWriter, r *http.Request) {
	_, _, last, _ := s.Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ranking":     "ok",
		"signal_lab":  "ok",
		"heatmap":     "ok",
		"netflow":     "ok",
		"last_ingest": last.UTC().Format(time.RFC3339),
	})
}

func (s *Service) handleRanking(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, s.Rank(limit))
}

func (s *Service) handleSignalLab(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request: symbol required"})
		return
	}
	s.TouchFlowSymbol(symbol)
	body, err := s.SignalLab(symbol)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (s *Service) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request: symbol required"})
		return
	}
	s.TouchFlowSymbol(symbol)
	body, err := s.Heatmap(symbol)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (s *Service) handleNetFlow(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "1h"
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	data, err := s.NetFlow(window, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// handlePriority accepts the engine's current candidate symbols (POST JSON
// {symbols:[...]}) or returns the current set (GET). The ingest worker uses
// these to prioritize Pineify enrichment so candidates carry fresh data.
func (s *Service) handlePriority(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		var body struct {
			Symbols []string `json:"symbols"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		s.SetPriority(body.Symbols)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "count": len(body.Symbols)})
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"symbols": s.Priority()})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
