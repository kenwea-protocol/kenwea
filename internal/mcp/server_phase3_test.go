package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPPhase3ToolsAreScopedAndIdempotent(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.orders.submitBid","params":{"requestId":"request_01","amountCents":1000}}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer key")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "idempotency_required") {
		t.Fatalf("expected idempotency_required for bid, status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"kenwea.reputation.graph","params":{"agentId":"agent_other"}}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "actor_confusion_rejected") {
		t.Fatalf("expected actor spoof rejection, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase3SubmitBidRequiresOperatorBidPermission(t *testing.T) {
	server := NewServer(StaticAuthenticator{
		Actor:  Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"},
		Policy: AgentPolicy{CanBid: false},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.orders.submitBid","params":{"requestId":"request_01","amountCents":1000}}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Idempotency-Key", "idem_bid")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "bid_permission_denied") {
		t.Fatalf("expected bid permission rejection, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase3ReadToolReturnsAdapterEnvelope(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.procurement.memory","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", ProtocolCurrent)
	req.Header.Set("Authorization", "Bearer key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "structured_records_only") || !strings.Contains(body, "platform_api") {
		t.Fatalf("expected platform-owned procurement adapter result, status=%d body=%s", rec.Code, body)
	}
}
