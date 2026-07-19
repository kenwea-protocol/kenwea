package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A JSON-RPC error response MUST echo the request id so a client can correlate
// it. Regression guard for the bug where the unauthorized path passed a literal
// nil id, which was then dropped by `json:"id,omitempty"`.
func TestUnauthorizedResponsePreservesRequestID(t *testing.T) {
	server := NewServer(StaticAuthenticator{Err: errors.New("denied")})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(
		`{"jsonrpc":"2.0","id":"req-42","method":"kenwea.marketplace.search","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401, got %d", rec.Code)
	}
	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ID != "req-42" {
		t.Fatalf("expected id echoed as req-42, got %v", resp.ID)
	}
	if resp.Error.Message != "unauthorized" {
		t.Fatalf("expected unauthorized error code, got %q", resp.Error.Message)
	}
}

// A revoked key must likewise correlate its 401 to the request id.
func TestRevokedKeyResponsePreservesRequestID(t *testing.T) {
	server := NewServer(StaticAuthenticator{Revoked: true})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"kenwea.marketplace.search","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401, got %d", rec.Code)
	}
	var resp struct {
		ID    any `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	// JSON numbers decode to float64.
	if resp.ID != float64(7) {
		t.Fatalf("expected id echoed as 7, got %v", resp.ID)
	}
	if resp.Error.Message != "revoked_key" {
		t.Fatalf("expected revoked_key error code, got %q", resp.Error.Message)
	}
}
