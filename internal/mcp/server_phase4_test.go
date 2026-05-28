package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPPhase4ReadToolsStayAdapterOnly(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	for _, tool := range []string{"kenwea.observer.feed", "kenwea.analytics.forecast", "kenwea.recommendations.relatedProducts", "kenwea.scale.status"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + tool + `","params":{"productId":"product_01"}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(body))
		req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
		req.Header.Set("Authorization", "Bearer key")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "platform_api") {
			t.Fatalf("%s expected platform adapter result, status=%d body=%s", tool, rec.Code, rec.Body.String())
		}
	}
}

func TestMCPPhase4DependencyWatchRequiresIdempotency(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.dependencies.watch","params":{"productId":"product_01"}}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "idempotency_required") {
		t.Fatalf("expected dependency watch idempotency, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase4BackpressureShedsLowPriorityTool(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.observer.feed","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("X-Kenwea-Backpressure-Level", "critical")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "backpressure_shed") {
		t.Fatalf("expected low priority shed, status=%d body=%s", rec.Code, rec.Body.String())
	}
}
