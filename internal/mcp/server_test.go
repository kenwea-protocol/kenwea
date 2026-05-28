package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPRejectsUnsupportedProtocolVersion(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2024-01-01")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unsupported_protocol_version") {
		t.Fatalf("expected deterministic protocol error, got %s", rec.Body.String())
	}
}

func TestMCPRejectsUnapprovedBrowserOrigin(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "origin_rejected") {
		t.Fatalf("expected origin rejection, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPGetSupportsEventStreamReadyEnvelope(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	req := httptest.NewRequest(http.MethodGet, "/mcp/v1", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected event stream ready response, status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "event: ready") {
		t.Fatalf("expected ready event, got %s", rec.Body.String())
	}
}

func TestMCPIdentityToolsRejectRevokedKey(t *testing.T) {
	server := NewServer(StaticAuthenticator{Revoked: true})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", bytes.NewBufferString(`{"jsonrpc":"2.0","id":"1","method":"kenwea.agent.identity","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "revoked_key") {
		t.Fatalf("expected revoked_key error, got %s", rec.Body.String())
	}
}

func TestMCPAllowsOnlyPhaseOneTools(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.purchase.create","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Fatalf("expected method_not_allowed, got %s", rec.Body.String())
	}
}

func TestMCPIssuesAndAcceptsSessionHeader(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if rec.Code != http.StatusOK || sessionID == "" {
		t.Fatalf("expected issued session header, status=%d session=%q body=%s", rec.Code, sessionID, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"kenwea.agent.heartbeat","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Mcp-Session-Id", sessionID)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session-authenticated request status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPDeleteRevokesSessionHeader(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	sessionID := rec.Header().Get("Mcp-Session-Id")

	req = httptest.NewRequest(http.MethodDelete, "/mcp/v1", nil)
	req.Header.Set("Mcp-Session-Id", sessionID)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Mcp-Session-Id", sessionID)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid_session") {
		t.Fatalf("expected invalid session after delete, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPSessionOnlyMutatingToolRequiresFreshAuthorization(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}, Policy: AgentPolicy{CanPublish: true, AllowDynamicPricing: true}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if rec.Code != http.StatusOK || sessionID == "" {
		t.Fatalf("expected issued session, status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"kenwea.marketplace.publish","params":{"title":"Tool","version":"1.0.0","summary":"Agent published tool","category":"automation","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true,"images":[{"url":"https://cdn.kenwea.example/tool.png","altText":"Tool preview"}]}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Mcp-Session-Id", sessionID)
	req.Header.Set("Idempotency-Key", "idem_publish")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "authorization_required") {
		t.Fatalf("expected fresh authorization for mutating session tool, status=%d body=%s", rec.Code, rec.Body.String())
	}
}
