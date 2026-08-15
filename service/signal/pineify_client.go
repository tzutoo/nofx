package signal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nofx/mcp"
)

// pineifyClient is a minimal Model Context Protocol (MCP) JSON-RPC client for
// the Pineify MCP server. It speaks just enough of the MCP wire protocol —
// initialize handshake + tools/call — to invoke Pineify's data tools from the
// ingest worker. It is NOT a general-purpose MCP client; it intentionally omits
// streaming/SSE, resource/prompt listing, and server-initiated notifications.
//
// MCP over HTTP is JSON-RPC 2.0: each request is a POST with an incrementing
// `id`, `method` ("initialize" | "tools/call") and `params`. The server may
// return the result inline as JSON, or (streamable-HTTP transport) as an SSE
// event carrying the same JSON-RPC envelope. This client handles both.
type pineifyClient struct {
	baseURL string
	token   string
	http    *http.Client
	log     mcp.Logger
	session string // MCP streamable-HTTP session id (may be empty for stateless JSON)
	nextID  int64
}

// newPineifyClient builds a client for the Pineify MCP endpoint.
func newPineifyClient(cfg *Config, httpClient *http.Client, log mcp.Logger) *pineifyClient {
	if log == nil {
		log = mcp.NewNoopLogger()
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &pineifyClient{
		baseURL: strings.TrimRight(cfg.PineifyBaseURL, "/"),
		token:   cfg.PineifyMCPToken,
		http:    httpClient,
		log:     log,
	}
}

// initialize performs the MCP `initialize` handshake and then the
// `notifications/initialized` notification. It is idempotent-safe to call on
// each ingest; a fresh session is cheap and avoids stale-session issues.
func (p *pineifyClient) initialize(ctx context.Context) error {
	resp := &mcpResponse{}
	headers := map[string]string{}
	if err := p.do(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "nofx-signal-service",
			"version": "1.0",
		},
	}, resp, headers); err != nil {
		return err
	}
	// Streamable-HTTP transport returns a session id header; remember it for
	// subsequent tools/call requests.
	if sid := headers["Mcp-Session-Id"]; sid != "" {
		p.session = sid
	}
	// Send the initialized notification (fire-and-forget; failure is non-fatal).
	_ = p.notify(ctx, "notifications/initialized", map[string]any{})
	return nil
}

// callTool invokes a named MCP tool with the given arguments and returns the
// tool result as a raw JSON payload. It prefers the tool's structuredContent
// (the canonical machine-readable data) and falls back to the prose text.
func (p *pineifyClient) callTool(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	resp := &mcpResponse{}
	if err := p.do(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, resp, nil); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp tools/call %s error: %s", name, resp.Error.Message)
	}
	return extractToolResult(resp.Result)
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (p *pineifyClient) notify(ctx context.Context, method string, params map[string]any) error {
	req := jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	p.decorate(httpReq, true)
	resp, err := p.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

// do sends a JSON-RPC request expecting a response envelope, decodes the
// response, and captures response headers (e.g. Mcp-Session-Id) if requested.
func (p *pineifyClient) do(ctx context.Context, method string, params map[string]any, out *mcpResponse, respHeaders map[string]string) error {
	p.nextID++
	req := jsonRPCRequest{JSONRPC: "2.0", ID: p.nextID, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	p.decorate(httpReq, true)

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcp %s request failed: %w", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mcp %s HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if respHeaders != nil {
		for k, v := range resp.Header {
			lower := strings.ToLower(k)
			if lower == "mcp-session-id" && len(v) > 0 {
				respHeaders["Mcp-Session-Id"] = v[0]
			}
		}
	}

	// Read and parse. The response is either plain JSON-RPC or an SSE stream
	// (streamable-HTTP) with one data: line carrying the JSON-RPC envelope.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("mcp %s read failed: %w", method, err)
	}
	if err := decodeMCPEnvelope(raw, out); err != nil {
		return fmt.Errorf("mcp %s decode failed: %w", method, err)
	}
	return nil
}

// decorate sets the common headers for a Pineify MCP request.
func (p *pineifyClient) decorate(r *http.Request, auth bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("X-Client-ID", "nofx")
	if auth && p.token != "" {
		r.Header.Set("Authorization", "Bearer "+p.token)
	}
	if p.session != "" {
		r.Header.Set("Mcp-Session-Id", p.session)
	}
}

// jsonRPCRequest is a JSON-RPC 2.0 request envelope.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// mcpResponse is a JSON-RPC 2.0 response envelope.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// decodeMCPEnvelope parses a raw response body as either plain JSON-RPC or an
// SSE-framed JSON-RPC envelope.
func decodeMCPEnvelope(raw []byte, out *mcpResponse) error {
	// Fast path: plain JSON.
	if err := json.Unmarshal(raw, out); err == nil {
		return nil
	}
	// SSE path: find the `data:` line(s) and concatenate their JSON.
	var sb strings.Builder
	scanner := bufioScanner(raw)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		sb.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	if sb.Len() == 0 {
		return fmt.Errorf("response is neither JSON-RPC nor SSE: %s", truncateBytes(raw, 300))
	}
	return json.Unmarshal([]byte(sb.String()), out)
}

// bufioScanner returns a bufio.Scanner over raw bytes.
func bufioScanner(raw []byte) *bufio.Scanner {
	return bufio.NewScanner(bytes.NewReader(raw))
}

// truncateBytes returns a short human-readable prefix of a byte slice.
func truncateBytes(raw []byte, max int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > max {
		return s[:max] + "...<truncated>"
	}
	return s
}

// extractToolResult pulls the machine-readable payload from a tool result.
// MCP tool results are `{ content:[{type:"text",text:"..."}], structuredContent:{...} }`.
// When structuredContent is present it is the canonical data and is returned
// verbatim; otherwise the joined text content is returned.
func extractToolResult(result json.RawMessage) (json.RawMessage, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("parse tool result envelope: %w", err)
	}
	if len(parsed.Structured) > 0 && string(parsed.Structured) != "null" {
		return parsed.Structured, nil
	}
	var sb strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return nil, fmt.Errorf("tool result has no content")
	}
	return json.RawMessage(sb.String()), nil
}
