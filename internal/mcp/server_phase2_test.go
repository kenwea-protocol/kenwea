package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPPhase2ToolsRequireIdempotencyForFinancialAndDestructiveCalls(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.purchase","params":{"productId":"prod_01"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "idempotency_required") {
		t.Fatalf("expected idempotency_required, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPRegisterSelfSkipsAgentAuthAndReturnsTouristCredentials(t *testing.T) {
	server := NewServerWithRuntime(StaticAuthenticator{Err: http.ErrNoCookie}, nil, nil, registerForwarder{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.onboarding.registerSelf","params":{"agentName":"Tourist Agent"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "agent_self") || !strings.Contains(body, "pair_self") || !strings.Contains(body, "touristMode") {
		t.Fatalf("expected tourist self-registration credentials, status=%d body=%s", rec.Code, body)
	}
}

func TestMCPUnboundAgentCanDiscoverButCannotPublish(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_self", AgentID: "agent_self"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.search","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer tourist")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected marketplace discovery for unbound agent, status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(validPublishJSON(2)))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer tourist")
	req.Header.Set("Idempotency-Key", "idem_publish")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Unbound Agent") {
		t.Fatalf("expected operator-required denial, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type registerForwarder struct{}

func (registerForwarder) ForwardTool(*http.Request, string, json.RawMessage) (map[string]any, error) {
	return map[string]any{
		"agent":       map[string]any{"agentId": "agent_self", "onboardingState": "unbound"},
		"apiKey":      map[string]any{"rawKey": "kw_tourist"},
		"pairingPin":  "pair_self",
		"touristMode": true,
	}, nil
}

type recordingForwarder struct {
	method string
	params json.RawMessage
	result map[string]any
}

func (f *recordingForwarder) ForwardTool(_ *http.Request, method string, params json.RawMessage) (map[string]any, error) {
	f.method = method
	f.params = params
	if f.result != nil {
		return f.result, nil
	}
	return map[string]any{"status": "forwarded_to_platform_api"}, nil
}

func TestMCPStartOperatorAgentForwardsToPlatformAndRequiresIdempotency(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "operator", ID: "op_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.onboarding.startOperatorAgent","params":{"agentName":"Procurement Sentinel"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_operator_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "idempotency_required") {
		t.Fatalf("expected idempotency_required, status=%d body=%s", rec.Code, rec.Body.String())
	}

	forwarder := &recordingForwarder{}
	server = NewServer(StaticAuthenticator{Actor: Actor{Type: "operator", ID: "op_01", OperatorID: "op_01"}})
	server.forwarder = forwarder
	req = httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"kenwea.onboarding.startOperatorAgent","params":{"agentName":"Procurement Sentinel"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_operator_test")
	req.Header.Set("Idempotency-Key", "idem_start_agent")
	rec = httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected forwarded start operator agent, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if forwarder.method != "kenwea.onboarding.startOperatorAgent" {
		t.Fatalf("expected forward call, got method=%q", forwarder.method)
	}
}

func TestMCPAgentHeartbeatForwardsToPlatformWithoutIdempotency(t *testing.T) {
	forwarder := &recordingForwarder{result: map[string]any{"status": "accepted"}}
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	server.forwarder = forwarder
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.agent.heartbeat","params":{}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "accepted") {
		t.Fatalf("expected forwarded heartbeat acceptance, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if forwarder.method != "kenwea.agent.heartbeat" {
		t.Fatalf("expected forward call, got method=%q", forwarder.method)
	}
}

func TestMCPPhase2AsyncPreviewReturnsDeterministicJobEnvelope(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.preview","params":{"productId":"prod_01"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"jobId", "traceId", "kenwea.jobs.getStatus", "poll"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("expected async envelope field %q in %s", want, rec.Body.String())
		}
	}
}

func TestMCPPhase2PublishRequiresProductImages(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.publish","params":{"title":"Capability","version":"1.0.0","summary":"Sandboxed utility","category":"automation_systems","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Idempotency-Key", "idem_publish")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "publish requires at least one product image") {
		t.Fatalf("expected product image validation, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase2PublishRejectsUnknownCategory(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.publish","params":{"title":"Unknown","version":"1.0.0","summary":"Unknown category","category":"physical_goods","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true,"images":[{"url":"https://cdn.kenwea.example/product.png","altText":"Product visual"}]}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Idempotency-Key", "idem_publish_unknown_category")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "approved digital product category") {
		t.Fatalf("expected category validation, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase2PublishAcceptsGameDevelopmentCategory(t *testing.T) {
	server := NewServer(StaticAuthenticator{
		Actor:  Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"},
		Policy: AgentPolicy{CanPublish: true},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.publish","params":{"title":"Game Prototype","version":"1.0.0","summary":"Playable prototype kit with NPC logic","category":"game_development","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true,"images":[{"url":"https://cdn.kenwea.example/game.png","altText":"Game prototype preview"}]}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Idempotency-Key", "idem_publish_game")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kenwea.jobs.getStatus") {
		t.Fatalf("expected game development publish to be accepted, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase2PublishAcceptsSpatialAssetCategory(t *testing.T) {
	server := NewServer(StaticAuthenticator{
		Actor:  Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"},
		Policy: AgentPolicy{CanPublish: true},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.publish","params":{"title":"3D Architecture Kit","version":"1.0.0","summary":"CAD floor plan, Blender scene, and Unity-ready textures","category":"3d_game_architecture","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true,"images":[{"url":"https://cdn.kenwea.example/spatial.png","altText":"3D architecture preview"}]}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Idempotency-Key", "idem_publish_spatial")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kenwea.jobs.getStatus") {
		t.Fatalf("expected spatial asset publish to be accepted, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPPhase2PublishDynamicPricingRequiresOperatorDelegation(t *testing.T) {
	server := NewServer(StaticAuthenticator{
		Actor:  Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"},
		Policy: AgentPolicy{CanPublish: true, AllowDynamicPricing: false},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.publish","params":{"title":"Capability","version":"1.0.0","summary":"Sandboxed utility","category":"automation_systems","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true,"allowDynamicPricing":true,"images":[{"url":"https://cdn.kenwea.example/product.png","altText":"Capability visual"}]}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Idempotency-Key", "idem_publish_dynamic")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "dynamic_pricing_denied") {
		t.Fatalf("expected dynamic pricing policy rejection, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func validPublishJSON(id int) string {
	return `{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"kenwea.marketplace.publish","params":{"title":"Capability","version":"1.0.0","summary":"Sandboxed utility","category":"automation_systems","license":"standard","artifactRef":"r2://artifact","sellerAgreementAccepted":true,"images":[{"url":"https://cdn.kenwea.example/product.png","altText":"Capability visual"}]}}`
}

func TestMCPPhase2RejectsActorSpoofForMarketplaceTools(t *testing.T) {
	server := NewServer(StaticAuthenticator{Actor: Actor{Type: "agent", ID: "agent_01", AgentID: "agent_01", OperatorID: "op_01"}})
	req := httptest.NewRequest(http.MethodPost, "/mcp/v1", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"kenwea.marketplace.install","params":{"agentId":"agent_other"}}`))
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer kw_agent_test")
	req.Header.Set("Idempotency-Key", "idem_install")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "actor_confusion_rejected") {
		t.Fatalf("expected actor confusion rejection, status=%d body=%s", rec.Code, rec.Body.String())
	}
}
