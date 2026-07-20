package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestMCPAllowsPublicMCPProductionOrigin(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Origin", "https://mcp.kenwea.com")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected production MCP origin to be allowed, status=%d body=%s", rec.Code, rec.Body.String())
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

func TestMCPStandardInitializeAndToolsList(t *testing.T) {
	server := NewServer(StaticAuthenticator{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0.1.0"}}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kenwea-public-mcp") || !strings.Contains(rec.Body.String(), "tools") {
		t.Fatalf("expected initialize envelope, status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":"tools","method":"tools/list","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec = httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kenwea.onboarding.registerSelf") || !strings.Contains(rec.Body.String(), "inputSchema") {
		t.Fatalf("expected tools/list envelope, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPStandardToolsCallRoutesToKenweaTool(t *testing.T) {
	server := NewServerWithRuntime(StaticAuthenticator{Err: http.ErrNoCookie}, nil, nil, registerForwarder{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":"call","method":"tools/call","params":{"name":"kenwea.onboarding.registerSelf","arguments":{"agentName":"Tourist Agent"}}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "structuredContent") || !strings.Contains(body, "agent_self") || !strings.Contains(body, "touristMode") {
		t.Fatalf("expected MCP tools/call result, status=%d body=%s", rec.Code, body)
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

func TestMCPSessionOnlyCommunityAskRequiresFreshAuthorization(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.auth.identify","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if rec.Code != http.StatusOK || sessionID == "" {
		t.Fatalf("expected issued session, status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"kenwea.community.ask","params":{"question":"How do sandboxes work?"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Mcp-Session-Id", sessionID)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "authorization_required") {
		t.Fatalf("expected fresh authorization for cached-session community.ask, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPIdentityToolsDoNotRequirePlatformForwarder(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	server.forwarder = failingForwarder{}
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.agent.identity","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "agent_01") {
		t.Fatalf("expected local identity adapter result, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type failingForwarder struct{}

func (failingForwarder) ForwardTool(*http.Request, string, json.RawMessage) (map[string]any, error) {
	return nil, errors.New("forwarder should not be called")
}

type erroringForwarder struct{ err error }

func (f erroringForwarder) ForwardTool(*http.Request, string, json.RawMessage) (map[string]any, error) {
	return nil, f.err
}

func searchRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.search","params":{"query":"trading"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_tourist")
	return req
}

// A platform client error (4xx) must reach the agent with the platform's own
// status and code, not be masked as a 502 outage that invites a pointless retry.
func TestForwardPlatformClientErrorPassesThroughStatusAndCode(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	server.forwarder = erroringForwarder{err: &PlatformError{StatusCode: http.StatusNotFound, Code: "product_not_found", Detail: "no such product"}}
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, searchRequest())

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 passthrough, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "product_not_found") || !strings.Contains(rec.Body.String(), "no such product") {
		t.Fatalf("expected platform code and detail, body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "platform_api_unavailable") {
		t.Fatalf("client error must not be reported as an outage, body=%s", rec.Body.String())
	}
}

// A transport failure (or any non-PlatformError) stays a 502 outage and must
// not leak the internal transport error text back to the caller.
func TestForwardPlatformOutageReportsGenericUnavailable(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	server.forwarder = erroringForwarder{err: errors.New("dial tcp 10.0.0.5:8080: connection refused")}
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, searchRequest())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "platform_api_unavailable") {
		t.Fatalf("expected platform_api_unavailable, body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection refused") || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("must not leak internal transport error to caller, body=%s", rec.Body.String())
	}
}

// A platform 5xx is a genuine outage and must map to 502, not pass through.
func TestForwardPlatformServerErrorReportsUnavailable(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	server.forwarder = erroringForwarder{err: &PlatformError{StatusCode: http.StatusInternalServerError, Code: "internal", Detail: "boom"}}
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, searchRequest())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for platform 5xx, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "platform_api_unavailable") {
		t.Fatalf("expected platform_api_unavailable, body=%s", rec.Body.String())
	}
}
